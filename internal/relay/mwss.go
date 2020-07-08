package relay

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gobwas/ws"
	"github.com/xtaci/smux"
)

type mwssTransporter struct {
	sessions     map[string][]*muxSession
	sessionMutex sync.Mutex
}

func NewMWSSTransporter() *mwssTransporter {
	return &mwssTransporter{
		sessions: make(map[string][]*muxSession),
	}
}

func (tr *mwssTransporter) Dial(addr string) (conn net.Conn, err error) {
	tr.sessionMutex.Lock()
	defer tr.sessionMutex.Unlock()

	var session *muxSession
	var sessionIndex int
	sessions, ok := tr.sessions[addr]

	// 找到可以用的session
	for sessionIndex, session = range sessions {
		if session.NumStreams() >= session.maxStreamCnt {
			ok = false
		} else {
			ok = true
			break
		}
	}

	// 删除已经关闭的session
	if session != nil && session.IsClosed() {
		Logger.Infof("find closed session %v idx: %d", session, sessionIndex)
		sessions = append(sessions[:sessionIndex], sessions[sessionIndex+1:]...)
		ok = false
	}

	// 创建新的session
	if !ok {
		session, err = tr.initSession(addr)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	cc, err := session.GetConn()
	if err != nil {
		session.Close()
		return nil, err
	}
	tr.sessions[addr] = sessions
	return cc, nil
}

func (tr *mwssTransporter) initSession(addr string) (*muxSession, error) {
	d := ws.Dialer{TLSConfig: DefaultTLSConfig}
	rc, _, _, err := d.Dial(context.TODO(), addr)
	if err != nil {
		return nil, err
	}
	wsc := NewDeadLinerConn(rc, WsDeadline)

	// stream multiplex
	smuxConfig := smux.DefaultConfig()
	session, err := smux.Client(wsc, smuxConfig)
	if err != nil {
		return nil, err
	}
	Logger.Infof("[mwss] Init new session %s", session.RemoteAddr())
	return &muxSession{
		conn: wsc, session: session, maxStreamCnt: MaxMWSSStreamCnt, t: WsDeadline}, nil
}

func (r *Relay) RunLocalMWSSServer() error {

	s := &MWSSServer{
		addr:     r.LocalTCPAddr.String(),
		connChan: make(chan net.Conn, 1024),
		errChan:  make(chan error, 1),
	}

	mux := http.NewServeMux()
	mux.Handle("/tcp/", http.HandlerFunc(s.upgrade))
	// fake
	mux.Handle("/", http.HandlerFunc(index))
	server := &http.Server{
		Addr:              r.LocalTCPAddr.String(),
		Handler:           mux,
		TLSConfig:         DefaultTLSConfig,
		ReadHeaderTimeout: 30 * time.Second,
	}
	s.server = server

	ln, err := net.Listen("tcp", r.LocalTCPAddr.String())
	if err != nil {
		return err
	}
	go func() {
		err := server.Serve(tls.NewListener(ln, server.TLSConfig))
		if err != nil {
			s.errChan <- err
		}
		close(s.errChan)
	}()

	var tempDelay time.Duration
	for {
		conn, e := s.Accept()
		if e != nil {
			if ne, ok := e.(net.Error); ok && ne.Temporary() {
				if tempDelay == 0 {
					tempDelay = 5 * time.Millisecond
				} else {
					tempDelay *= 2
				}
				if max := 1 * time.Second; tempDelay > max {
					tempDelay = max
				}
				Logger.Infof("server: Accept error: %v; retrying in %v", e, tempDelay)
				time.Sleep(tempDelay)
				continue
			}
			return e
		}
		tempDelay = 0

		go r.handleMWSSConnToTcp(conn)
	}
}

type MWSSServer struct {
	addr     string
	server   *http.Server
	connChan chan net.Conn
	errChan  chan error
}

func (s *MWSSServer) upgrade(w http.ResponseWriter, r *http.Request) {
	conn, _, _, err := ws.UpgradeHTTP(r, w)
	if err != nil {
		Logger.Info(err)
		return
	}
	s.mux(NewDeadLinerConn(conn, WsDeadline))
}

func (s *MWSSServer) mux(conn net.Conn) {
	smuxConfig := smux.DefaultConfig()
	mux, err := smux.Server(conn, smuxConfig)
	if err != nil {
		Logger.Infof("[mwss] %s - %s : %s", conn.RemoteAddr(), s.Addr(), err)
		return
	}
	defer mux.Close()

	Logger.Infof("[mwss] %s <-> %s", conn.RemoteAddr(), s.Addr())
	defer Logger.Infof("[mwss] %s >-< %s", conn.RemoteAddr(), s.Addr())

	failedCount := 0
	for failedCount < 5 {
		stream, err := mux.AcceptStream()
		if err != nil {
			Logger.Infof("[mwss] accept stream err: %s failedCount: %d", err, failedCount)
			failedCount++
			break
		}
		cc := newMuxDeadlineStreamConn(conn, stream, WsDeadline)
		select {
		case s.connChan <- cc:
		default:
			cc.Close()
			Logger.Infof("[mwss] %s - %s: connection queue is full", conn.RemoteAddr(), conn.LocalAddr())
		}
	}
}

func (s *MWSSServer) Accept() (conn net.Conn, err error) {
	select {
	case conn = <-s.connChan:
	case err = <-s.errChan:
	}
	return
}

func (s *MWSSServer) Close() error {
	return s.server.Close()
}

func (s *MWSSServer) Addr() string {
	return s.addr
}

var tr = NewMWSSTransporter()

func (r *Relay) handleTcpOverMWSS(c *net.TCPConn) error {
	dc := NewDeadLinerConn(c, TcpDeadline)
	defer dc.Close()

	addr := r.RemoteTCPAddr + "/tcp/"
	wsc, err := tr.Dial(addr)
	if err != nil {
		return err
	}
	defer wsc.Close()
	Logger.Infof("handleTcpOverMWSS from:%s to:%s", c.RemoteAddr(), wsc.RemoteAddr())
	transport(wsc, dc)
	return nil
}

func (r *Relay) handleMWSSConnToTcp(c net.Conn) {
	defer c.Close()
	rc, err := net.Dial("tcp", r.RemoteTCPAddr)
	if err != nil {
		Logger.Infof("dial error: %s", err)
		return
	}
	drc := NewDeadLinerConn(rc, TcpDeadline)
	defer drc.Close()

	Logger.Infof("handleMWSSConnToTcp from:%s to:%s", c.RemoteAddr(), rc.RemoteAddr())
	transport(drc, c)
}