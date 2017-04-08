package client

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/longXboy/Lunnel/crypto"
	"github.com/longXboy/Lunnel/log"
	"github.com/longXboy/Lunnel/msg"
	"github.com/longXboy/Lunnel/transport"
	"github.com/longXboy/Lunnel/util"
	"github.com/longXboy/smux"
	"github.com/pkg/errors"
)

var pingInterval time.Duration = time.Second * 30
var pingTimeout time.Duration = time.Second * 70

func NewControl(conn net.Conn, encryptMode string, transport string) *Control {
	ctl := &Control{
		ctlConn:       conn,
		die:           make(chan struct{}),
		toDie:         make(chan struct{}),
		writeChan:     make(chan writeReq, 128),
		encryptMode:   encryptMode,
		tunnels:       make(map[string]msg.TunnelConfig, 0),
		transportMode: transport,
	}
	return ctl
}

type writeReq struct {
	mType msg.MsgType
	body  interface{}
}

type Control struct {
	ctlConn         net.Conn
	tunnelLock      sync.Mutex
	tunnels         map[string]msg.TunnelConfig
	preMasterSecret []byte
	lastRead        uint64
	encryptMode     string
	transportMode   string
	totalPipes      int64

	die       chan struct{}
	toDie     chan struct{}
	writeChan chan writeReq

	ClientID crypto.UUID
}

func (c *Control) Close() {
	c.toDie <- struct{}{}
	log.WithField("time", time.Now().UnixNano()).Debugln("control closing")
	return
}

func (c *Control) IsClosed() bool {
	select {
	case <-c.die:
		return true
	default:
		return false
	}
}

func (c *Control) moderator() {
	_ = <-c.toDie
	close(c.die)
	c.ctlConn.Close()
}

func (c *Control) createPipe() {
	log.WithFields(log.Fields{"time": time.Now().Unix(), "pipe_count": atomic.LoadInt64(&c.totalPipes)}).Debugln("create pipe to server!")
	pipeConn, err := transport.CreateConn(cliConf.ServerAddr, c.transportMode, cliConf.HttpProxy)
	if err != nil {
		log.WithFields(log.Fields{"addr": cliConf.ServerAddr, "err": err}).Errorln("creating tunnel conn to server failed!")
		return
	}
	defer pipeConn.Close()

	pipe, err := c.pipeHandShake(pipeConn)
	if err != nil {
		pipeConn.Close()
		log.WithFields(log.Fields{"err": err}).Errorln("pipeHandShake failed!")
		return
	}
	defer pipe.Close()
	atomic.AddInt64(&c.totalPipes, 1)
	defer func() {
		log.WithFields(log.Fields{"pipe_count": atomic.LoadInt64(&c.totalPipes)}).Debugln("total pipe count")
		atomic.AddInt64(&c.totalPipes, -1)
	}()
	for {
		if c.IsClosed() {
			return
		}
		if pipe.IsClosed() {
			return
		}
		stream, err := pipe.AcceptStream()
		if err != nil {
			log.WithFields(log.Fields{"err": err, "time": time.Now().Unix()}).Warningln("pipeAcceptStream failed!")
			return
		}
		go func() {
			defer stream.Close()
			c.tunnelLock.Lock()
			tunnel, isok := c.tunnels[stream.TunnelName()]
			c.tunnelLock.Unlock()
			if !isok {
				log.WithFields(log.Fields{"name": stream.TunnelName()}).Errorln("can't find tunnel by name")
				return
			}
			var conn net.Conn
			localProto, hostname, port, err := util.ParseLocalAddr(tunnel.LocalAddr)
			if err != nil {
				log.WithFields(log.Fields{"err": err, "local": tunnel.LocalAddr}).Errorln("util.ParseLocalAddr failed!")
				return
			}
			if localProto == "http" || localProto == "https" || localProto == "" {
				if port == "" {
					if localProto == "https" {
						port = "443"
					} else {
						port = "80"
					}
				}
				conn, err = net.Dial("tcp", fmt.Sprintf("%s:%s", hostname, port))
				if err != nil {
					log.WithFields(log.Fields{"err": err, "local": tunnel.LocalAddr}).Warningln("pipe dial local failed!")
					return
				}
				if tunnel.Protocol == "https" {
					conn = tls.Client(conn, &tls.Config{InsecureSkipVerify: true})
				}
			} else if localProto == "unix" {
				conn, err = net.Dial("unix", hostname)
				if err != nil {
					log.WithFields(log.Fields{"err": err, "local": tunnel.LocalAddr}).Warningln("pipe dial local failed!")
					return
				}
			} else {
				if port == "" {
					log.WithFields(log.Fields{"err": fmt.Sprintf("no port sepicified"), "local": tunnel.LocalAddr}).Errorln("dial local addr failed!")
					return
				}
				conn, err = net.Dial(localProto, hostname)
				if err != nil {
					log.WithFields(log.Fields{"err": err, "local": tunnel.LocalAddr}).Warningln("pipe dial local failed!")
					return
				}
			}
			defer conn.Close()

			p1die := make(chan struct{})
			p2die := make(chan struct{})

			go func() {
				io.Copy(stream, conn)
				close(p1die)
			}()
			go func() {
				io.Copy(conn, stream)
				close(p2die)
			}()
			select {
			case <-p1die:
			case <-p2die:
			}
		}()
	}
}

func (c *Control) SyncTunnels(cstm *msg.AddTunnels) error {
	for k, v := range cstm.Tunnels {
		c.tunnelLock.Lock()
		c.tunnels[k] = v
		c.tunnelLock.Unlock()
		log.WithFields(log.Fields{"local": v.LocalAddr, "remote": v.RemoteAddr()}).Infoln("client sync tunnel complete")
	}
	return nil
}

func (c *Control) ClientAddTunnels() error {
	cstm := new(msg.AddTunnels)
	cstm.Tunnels = cliConf.Tunnels
	err := msg.WriteMsg(c.ctlConn, msg.TypeAddTunnels, *cstm)
	if err != nil {
		return errors.Wrap(err, "WriteMsg cstm")
	}
	return nil
}

func (c *Control) recvLoop() {
	atomic.StoreUint64(&c.lastRead, uint64(time.Now().UnixNano()))
	for {
		if c.IsClosed() {
			return
		}
		mType, body, err := msg.ReadMsgWithoutTimeout(c.ctlConn)
		if err != nil {
			log.WithFields(log.Fields{"err": err, "client_id": c.ClientID.Hex()}).Warningln("ReadMsgWithoutTimeout in recv loop failed")
			c.Close()
			return
		}
		log.WithFields(log.Fields{"mType": mType}).Debugln("recv msg from server")
		atomic.StoreUint64(&c.lastRead, uint64(time.Now().UnixNano()))
		switch mType {
		case msg.TypePong:
		case msg.TypePing:
			c.writeChan <- writeReq{msg.TypePong, nil}
		case msg.TypePipeReq:
			go c.createPipe()
		case msg.TypeAddTunnels:
			c.SyncTunnels(body.(*msg.AddTunnels))
		case msg.TypeError:
			log.Errorln("recv server error:", body.(*msg.Error).Error())
			c.Close()
			return
		}
	}
}

func (c *Control) writeLoop() {
	lastWrite := time.Now()
	for {
		if c.IsClosed() {
			return
		}
		select {
		case msgBody := <-c.writeChan:
			if msgBody.mType == msg.TypePing || msgBody.mType == msg.TypePong {
				if time.Now().Before(lastWrite.Add(pingInterval / 2)) {
					continue
				}
			}
			lastWrite = time.Now()
			err := msg.WriteMsg(c.ctlConn, msgBody.mType, msgBody.body)
			if err != nil {
				log.WithFields(log.Fields{"mType": msgBody.mType, "body": fmt.Sprintf("%v", msgBody.body), "client_id": c.ClientID.Hex(), "err": err}).Warningln("send msg to server failed!")
				c.Close()
				return
			}
		case _ = <-c.die:
			return
		}
	}

}

func (c *Control) listenAndStop() {
	sigChan := make(chan os.Signal)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL)
	select {
	case s := <-sigChan:
		log.WithFields(log.Fields{"signal": s.String(), "client_id": c.ClientID.Hex()}).Infoln("got signal to stop")
		c.Close()
		time.Sleep(time.Millisecond * 300)
		os.Exit(1)
	case <-c.die:
		signal.Reset()
	}
}

func (c *Control) Run() {
	go c.moderator()
	go c.recvLoop()
	go c.writeLoop()
	go c.listenAndStop()

	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if (uint64(time.Now().UnixNano()) - atomic.LoadUint64(&c.lastRead)) > uint64(pingTimeout) {
				log.WithFields(log.Fields{"client_id": c.ClientID.Hex()}).Warningln("recv server ping time out!")
				c.Close()
				return
			}
			select {
			case c.writeChan <- writeReq{msg.TypePing, nil}:
			case _ = <-c.die:
				return
			}
		case <-c.die:
			return
		}
	}
}

func (c *Control) ClientHandShake() error {
	var ckem msg.ControlClientHello
	var priv []byte
	var keyMsg []byte
	if c.encryptMode != "none" {
		priv, keyMsg = crypto.GenerateKeyExChange()
		if keyMsg == nil || priv == nil {
			return errors.New("GenerateKeyExChange error,key is nil")
		}
		ckem.CipherKey = keyMsg
	}
	ckem.AuthToken = cliConf.AuthToken
	err := msg.WriteMsg(c.ctlConn, msg.TypeControlClientHello, ckem)
	if err != nil {
		return errors.Wrap(err, "WriteMsg ckem")
	}

	mType, body, err := msg.ReadMsg(c.ctlConn)
	if err != nil {
		return errors.Wrap(err, "read ClientID")
	}
	if mType == msg.TypeError {
		err := body.(*msg.Error)
		return errors.Wrap(err, "read ClientID")
	}
	cidm := body.(*msg.ControlServerHello)
	c.ClientID = cidm.ClientID
	if len(cidm.CipherKey) > 0 {
		preMasterSecret, err := crypto.ProcessKeyExchange(priv, cidm.CipherKey)
		if err != nil {
			return errors.Wrap(err, "crypto.ProcessKeyExchange")
		}
		c.preMasterSecret = preMasterSecret
	}
	return nil
}

func (c *Control) pipeHandShake(conn net.Conn) (*smux.Session, error) {
	var phs msg.PipeClientHello
	phs.Once = crypto.GenUUID()
	phs.ClientID = c.ClientID
	err := msg.WriteMsg(conn, msg.TypePipeClientHello, phs)
	if err != nil {
		return nil, errors.Wrap(err, "write pipe handshake")
	}
	smuxConfig := smux.DefaultConfig()
	smuxConfig.MaxReceiveBuffer = 4194304
	var mux *smux.Session
	if c.encryptMode != "none" {
		prf := crypto.NewPrf12()
		var masterKey []byte = make([]byte, 16)
		prf(masterKey, c.preMasterSecret, c.ClientID[:], phs.Once[:])
		cryptoConn, err := crypto.NewCryptoConn(conn, masterKey)
		if err != nil {
			return nil, errors.Wrap(err, "crypto.NewCryptoConn")
		}

		mux, err = smux.Server(cryptoConn, smuxConfig)
		if err != nil {
			return nil, errors.Wrap(err, "smux.Server")
		}
	} else {
		mux, err = smux.Server(conn, smuxConfig)
		if err != nil {
			return nil, errors.Wrap(err, "smux.Server")
		}
	}

	return mux, nil
}
