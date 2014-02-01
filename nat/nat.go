package nat

import (
	"./stun"
	"errors"
	"log"
	"net"
	"strings"
	"time"
)

func Init(outIpList string, buster bool) (*AttemptEngine, error) {
	sock, err := net.ListenUDP("udp", &net.UDPAddr{})
	if err != nil {
		return nil, err
	}

	engine := &AttemptEngine{sock: sock, buster: buster}
	if err := engine.init(outIpList); err != nil {
		return nil, err
	}
	return engine, nil
}

type attempt struct {
	candidate
	tid       []byte
	timeout   time.Time
	success   bool // did we get a STUN response from this addr
	chosen    bool // Has this channel been picked for the connection?
	localaddr net.Addr
}

type AttemptEngine struct {
	buster         bool
	sock           *net.UDPConn
	attempts       []attempt
	local_attempts []attempt
	p2pconn        net.Conn
	otherReady     bool
	status         string
}

const probeTimeout = 500 * time.Millisecond
const probeInterval = 100 * time.Millisecond
const decisionTime = 2 * time.Second

func (e *AttemptEngine) SetOtherAddrList(addrList string) {
	arr := strings.Split(addrList, "\n")
	e.attempts = make([]attempt, 0)
	for _, addrStr := range arr {
		if addrStr != "" {
			addr, err := net.ResolveUDPAddr("udp", addrStr)
			if err != nil {
				log.Println("resolve udp addr err", err.Error())
			} else {
				e.attempts = append(e.attempts, attempt{candidate: candidate{Addr: addr}})
			}
		}
	}
}

func (e *AttemptEngine) GetAddrList() string {
	tmp := ""
	for _, attempt := range e.local_attempts {
		tmp += attempt.Addr.String() + "\n"
	}
	return tmp
}

func (e *AttemptEngine) Fail() {
	e.status = "quit"
	if e.sock != nil {
		//log.Println("close udp sock")
		e.sock.Close()
	}
}

func (e *AttemptEngine) GetConn(f func()) (net.Conn, error) {
	var conn net.Conn
	var err error
	if conn, err = e.run(f); err != nil {
		return nil, err
	}
	return conn, nil
}

func (e *AttemptEngine) init(outIpList string) error {
	candidates, err := GatherCandidates(e.sock, outIpList)
	if err != nil {
		return err
	}
	e.local_attempts = make([]attempt, len(candidates))
	for i := range candidates {
		e.local_attempts[i].candidate = candidates[i]
		log.Println("init addr", candidates[i].Addr.String())
	}

	e.sock.SetWriteDeadline(time.Time{})

	return nil
}

func (e *AttemptEngine) xmit() (time.Time, error) {
	now := time.Now()
	var ret time.Time
	var err error

	if e.p2pconn != nil {
		return ret, nil
	}

	for i := range e.attempts {
		if e.attempts[i].timeout.Before(now) {
			e.attempts[i].timeout = time.Now().Add(probeTimeout)
			e.attempts[i].tid, err = stun.RandomTid()
			if err != nil {
				return time.Time{}, err
			}
			packet, err := stun.BindRequest(e.attempts[i].tid, e.attempts[i].Addr, nil, false, e.attempts[i].chosen)
			if err != nil {
				return time.Time{}, err
			}
			//log.Println("===send", i,e.attempts[i].Addr.String())
			e.sock.WriteToUDP(packet, e.attempts[i].Addr)

			for j := range e.local_attempts {
				if e.local_attempts[j].success {
					packet, err := stun.BindRequest(e.attempts[i].tid, e.attempts[i].Addr, nil, false, e.attempts[i].chosen)
					if err != nil {
						return time.Time{}, err
					}
					//log.Println("===send local", i,e.local_attempts[j].localaddr.String())
					e.sock.WriteToUDP(packet, e.local_attempts[j].localaddr.(*net.UDPAddr))
				}
			}
		}
		if ret.IsZero() || e.attempts[i].timeout.Before(ret) {
			ret = e.attempts[i].timeout
		}
	}
	return ret, nil
}

func (e *AttemptEngine) read() error {
	if e.status == "over" {
		return nil
	}
	buf := make([]byte, 512)
	n, from, err := e.sock.ReadFromUDP(buf)
	//println("read", n, from, err)
	if err != nil {
		if neterr, ok := err.(net.Error); ok && neterr.Timeout() {
			return nil
		}
		return err
	}
	if string(buf[0:n]) == "makeholeover" {
		if e.status == "wait" {
			log.Println("wait client !!!!!!! close")
			e.status = "over"
			e.sock.WriteToUDP([]byte("makeholeover2"), from)
		}
		return nil
	}

	if string(buf[0:n]) == "makeholeover2" {
		if e.status == "wait" {
			log.Println("wait server !!!!!!! close")
			e.status = "over"
		}
		return nil
	}

	//log.Println("========", string(buf[0:n]))
	packet, err := stun.ParsePacket(buf[:n], nil)
	if err != nil {
		return nil
	}

	if packet.Method != stun.MethodBinding {
		return nil
	}

	validAddr := packet.Addr
	for i := range e.local_attempts {
		my_local_addr := e.local_attempts[i].Addr
		//log.Println("check local",i, validAddr.String(), packet.Class, from.String(), my_local_addr.String())
		if validAddr.String() == my_local_addr.String() {
			e.local_attempts[i].localaddr = from
			e.local_attempts[i].success = true
			//log.Println("find the addr from request", packet.Class, from.String())
			if packet.Class == stun.ClassRequest {
				for j := range e.attempts {
					my_remote_addr := e.attempts[j].Addr
					response, err := stun.BindResponse(packet.Tid[:], my_remote_addr, nil, false)
					if err != nil {
						return nil
					}
					log.Println("write to succ", from.String(), j, my_remote_addr.String())
					e.sock.WriteToUDP(response, from)
				}
			} else if packet.Class == stun.ClassSuccess {
				if e.p2pconn == nil {
					log.Println("make conn success", from.String(), e.local_attempts[i].localaddr.String())
					e.p2pconn = newConn(e.sock, e.local_attempts[i].Addr, e.local_attempts[i].localaddr)
					for j := range e.attempts {
						my_remote_addr := e.attempts[j].Addr
						response, err := stun.InformReady(packet.Tid[:], my_remote_addr, nil)
						if err != nil {
							return nil
						}
						log.Println("write to ready", from.String(), j, my_remote_addr.String())
						e.sock.WriteToUDP(response, from)
					}
				}
			} else if packet.Class == stun.ClassIndication {
				log.Println("recv other ready")
				e.otherReady = true
				/*	for j := range e.attempts {
					log.Println("write !!!!!!", from.String(),j)
					e.sock.WriteToUDP([]byte("wocao,okokokook1!!"), from)
				}*/
			} else if packet.Class == stun.ClassError {
				//			log.Println("!!!!!!!!!!!!!")
			}
		}
	}

	return nil
}

func (e *AttemptEngine) run(f func()) (net.Conn, error) {
	bInform := false
	beginTime := time.Now().Unix()
	for {
		if time.Now().Unix()-beginTime > 10 {
			e.status = "fail"
		}
		if e.status == "fail" || e.status == "quit" {
			break
		}
		timeout, err := e.xmit()
		if err != nil {
			return nil, err
		}
		if !bInform {
			f()
			bInform = true
			beginTime = time.Now().Unix()
		}
		e.sock.SetReadDeadline(timeout)
		if err = e.read(); err != nil {
			//return nil, err
		}
		if e.p2pconn != nil && e.otherReady {
			if e.buster && e.status == "" {
				log.Println("write final!!!!!!")
				e.sock.WriteToUDP([]byte("makeholeover"), e.p2pconn.RemoteAddr().(*net.UDPAddr))
			}
			if e.status != "over" {
				e.status = "wait"
			}
		}
		if e.status == "over" {
			return e.p2pconn, nil
		}
	}

	return nil, errors.New(e.status)
}
