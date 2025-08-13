package client

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	pbGlobal "github.com/east-eden/server/proto/global"
	"github.com/east-eden/server/transport"
	"github.com/east-eden/server/utils"
	log "github.com/rs/zerolog/log"
	"github.com/urfave/cli/v2"
	"google.golang.org/protobuf/proto"
)

type GateInfo struct {
	UserID        string `json:"userId"`
	UserName      string `json:"userName"`
	AccountID     int64  `json:"accountId"`
	GateID        string `json:"gateId"`
	PublicTcpAddr string `json:"publicTcpAddr"`
	PublicWsAddr  string `json:"publicWsAddr"`
}

type TransportClient struct {
	c       *Client
	tr      transport.Transport
	ts      transport.Socket
	wg      utils.WaitGroupWrapper
	wgRecon utils.WaitGroupWrapper

	gateInfo *GateInfo
	tlsConf  *tls.Config

	protocol       string
	connected      int32
	needReconnect  int32
	cancelRecvSend context.CancelFunc
	chDisconnect   chan int
	returnMsgName  chan string
	unProcedMsg    int32

	ticker *time.Ticker
	chSend chan proto.Message
	sync.Mutex
}

func NewTransportClient(ctx *cli.Context, c *Client) *TransportClient {

	t := &TransportClient{
		c:              c,
		returnMsgName:  make(chan string, 100),
		ticker:         time.NewTicker(ctx.Duration("heart_beat")),
		chDisconnect:   make(chan int, 1),
		needReconnect:  0,
		connected:      0,
		cancelRecvSend: func() {},
		chSend:         make(chan proto.Message, 64),
	}

	// // tls
	// var certFile, keyFile string
	// if ctx.Bool("debug") {
	// 	certFile = ctx.String("cert_path_debug")
	// 	keyFile = ctx.String("key_path_debug")
	// } else {
	// 	certFile = ctx.String("cert_path_release")
	// 	keyFile = ctx.String("key_path_release")
	// }

	// t.tlsConf = &tls.Config{InsecureSkipVerify: true}
	// cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	// if err != nil {
	// 	log.Fatal().Err(err).Msg("load certificates failed")
	// }

	// t.tlsConf.Certificates = []tls.Certificate{cert}

	// timer heart beat
	go func() {
		defer utils.CaptureException()

		for {
			select {
			case <-t.ticker.C:
				if atomic.LoadInt32(&t.connected) == 0 {
					continue
				}

				t.sendHeartBeat()

			default:
				time.Sleep(time.Millisecond * 100)
			}
		}

	}()

	return t
}

func (t *TransportClient) connect(ctx context.Context) error {
	connector := &TransportConnector{
		client: t,
		ctx:    ctx,
	}

	return connector.Execute()
}

// TransportConnector handles the complete connection establishment process
type TransportConnector struct {
	client *TransportClient
	ctx    context.Context
}

// Execute performs the complete connection sequence
func (c *TransportConnector) Execute() error {
	log.Info().Int64("client_id", c.client.c.Id).
		Str("protocol", c.client.protocol).
		Msg("starting transport connection process")

	// Step 1: Establish network connection
	if err := c.establishNetworkConnection(); err != nil {
		return fmt.Errorf("failed to establish network connection: %w", err)
	}

	// Step 2: Initialize connection state
	c.initializeConnectionState()

	// Step 3: Initialize communication channel
	c.initializeCommunicationChannel()

	// Step 4: Perform handshake and authentication
	if err := c.performHandshakeAndAuth(); err != nil {
		return fmt.Errorf("failed to perform handshake and auth: %w", err)
	}

	// Step 5: Start message processing goroutines
	c.startMessageProcessing()

	log.Info().Int64("client_id", c.client.c.Id).
		Msg("transport connection established successfully")

	return nil
}

// establishNetworkConnection creates the network connection to the server
func (c *TransportConnector) establishNetworkConnection() error {
	serverAddr := c.resolveServerAddress()

	log.Debug().Int64("client_id", c.client.c.Id).
		Str("server_addr", serverAddr).
		Str("protocol", c.client.protocol).
		Msg("dialing to server")

	ts, err := c.client.tr.Dial(serverAddr)
	if err != nil {
		log.Error().Int64("client_id", c.client.c.Id).
			Str("server_addr", serverAddr).
			Err(err).
			Msg("failed to dial server")
		return err
	}

	c.client.ts = ts

	log.Info().Int64("client_id", c.client.c.Id).
		Str("local_addr", ts.Local()).
		Str("remote_addr", ts.Remote()).
		Str("protocol", c.client.protocol).
		Msg("network connection established")

	return nil
}

// resolveServerAddress determines the correct server address based on protocol
func (c *TransportConnector) resolveServerAddress() string {
	switch c.client.protocol {
	case "ws":
		return "wss://" + c.client.gateInfo.PublicWsAddr
	case "tcp":
		fallthrough
	default:
		return c.client.gateInfo.PublicTcpAddr
	}
}

// initializeConnectionState sets up the connection state flags
func (c *TransportConnector) initializeConnectionState() {
	atomic.StoreInt32(&c.client.connected, 1)

	log.Debug().Int64("client_id", c.client.c.Id).
		Msg("connection state initialized")
}

// initializeCommunicationChannel creates the message sending channel
func (c *TransportConnector) initializeCommunicationChannel() {
	const channelBufferSize = 100
	c.client.chSend = make(chan proto.Message, channelBufferSize)

	log.Debug().Int64("client_id", c.client.c.Id).
		Int("buffer_size", channelBufferSize).
		Msg("communication channel initialized")
}

// performHandshakeAndAuth sends handshake and login messages
func (c *TransportConnector) performHandshakeAndAuth() error {
	log.Debug().Int64("client_id", c.client.c.Id).
		Msg("performing handshake and authentication")

	// Send handshake message
	if err := c.sendHandshakeMessage(); err != nil {
		return fmt.Errorf("handshake failed: %w", err)
	}

	// Send login message
	if err := c.sendLoginMessage(); err != nil {
		return fmt.Errorf("login failed: %w", err)
	}

	log.Debug().Int64("client_id", c.client.c.Id).
		Msg("handshake and authentication completed")

	return nil
}

// sendHandshakeMessage creates and sends the handshake message
func (c *TransportConnector) sendHandshakeMessage() error {
	handshakeMsg := &pbGlobal.Handshake{
		ConnType:     pbGlobal.ConnType_New,
		MsgType:      pbGlobal.MsgType_Direct,
		ClientAddr:   c.client.ts.Local(),
		UserId:       c.client.gateInfo.UserID,
		ClientVer:    "0.0.1",
		ClientResVer: "0.0.1",
		Metadata:     make(map[string]string),
	}

	log.Debug().Int64("client_id", c.client.c.Id).
		Str("user_id", c.client.gateInfo.UserID).
		Str("client_addr", c.client.ts.Local()).
		Msg("sending handshake message")

	c.client.chSend <- handshakeMsg
	return nil
}

// sendLoginMessage creates and sends the login message
func (c *TransportConnector) sendLoginMessage() error {
	loginMsg := &pbGlobal.C2S_AccountLogon{
		UserId:      c.client.gateInfo.UserID,
		AccountId:   c.client.gateInfo.AccountID,
		AccountName: c.client.gateInfo.UserName,
	}

	log.Info().Int64("client_id", c.client.c.Id).
		Str("user_id", c.client.gateInfo.UserID).
		Int64("account_id", c.client.gateInfo.AccountID).
		Str("account_name", c.client.gateInfo.UserName).
		Msg("sending login message")

	c.client.chSend <- loginMsg
	return nil
}

// startMessageProcessing starts the send and receive goroutines
func (c *TransportConnector) startMessageProcessing() {
	log.Debug().Int64("client_id", c.client.c.Id).
		Msg("starting message processing goroutines")

	// Start message sending goroutine
	c.startSendGoroutine()
	q
	// Start message receiving goroutine
	c.startReceiveGoroutine()

	log.Debug().Int64("client_id", c.client.c.Id).
		Msg("message processing goroutines started")
}

// startSendGoroutine starts the message sending goroutine
func (c *TransportConnector) startSendGoroutine() {
	c.client.wg.Wrap(func() {
		defer utils.CaptureException()

		log.Debug().Int64("client_id", c.client.c.Id).
			Msg("message send goroutine started")

		err := c.client.onSend(c.ctx)
		if err != nil {
			log.Warn().Int64("client_id", c.client.c.Id).
				Err(err).
				Msg("message send goroutine finished with error")
			atomic.StoreInt32(&c.client.needReconnect, 1)
		} else {
			log.Debug().Int64("client_id", c.client.c.Id).
				Msg("message send goroutine finished normally")
		}
	})
}

// startReceiveGoroutine starts the message receiving goroutine
func (c *TransportConnector) startReceiveGoroutine() {
	c.client.wg.Wrap(func() {
		defer utils.CaptureException()

		log.Debug().Int64("client_id", c.client.c.Id).
			Msg("message receive goroutine started")

		err := c.client.onRecv(c.ctx)
		if err != nil {
			log.Warn().Int64("client_id", c.client.c.Id).
				Err(err).
				Msg("message receive goroutine finished with error")
			atomic.StoreInt32(&c.client.needReconnect, 1)
		} else {
			log.Debug().Int64("client_id", c.client.c.Id).
				Msg("message receive goroutine finished normally")
		}
	})
}

// sendHandshake sends a handshake message to the server
// Deprecated: Use TransportConnector.sendHandshakeMessage() instead
func (t *TransportClient) sendHandshake() {
	connector := &TransportConnector{client: t}
	if err := connector.sendHandshakeMessage(); err != nil {
		log.Error().Int64("client_id", t.c.Id).Err(err).Msg("failed to send handshake")
	}
}

// sendLogon sends a login message to the server
// Deprecated: Use TransportConnector.sendLoginMessage() instead
func (t *TransportClient) sendLogon() {
	connector := &TransportConnector{client: t}
	if err := connector.sendLoginMessage(); err != nil {
		log.Error().Int64("client_id", t.c.Id).Err(err).Msg("failed to send login")
	}
}

func (t *TransportClient) sendHeartBeat() {
	msg := &pbGlobal.C2S_HeartBeat{}
	t.chSend <- msg
}

func (t *TransportClient) StartConnect(ctx context.Context) error {
	if t.tr != nil {
		return errors.New("TransportClient.StartConnect failed: connection existed")
	}

	if t.protocol == "tcp" {
		t.tr = transport.NewTransport("tcp")
		t.tr.Init(
			transport.Timeout(transport.DefaultDialTimeout),
		)
	} else {
		t.tr = transport.NewTransport("ws")
		t.tr.Init(
			transport.Timeout(transport.DefaultDialTimeout),
			transport.TLSConfig(t.tlsConf),
		)
	}

	t.wgRecon.Wrap(func() {
		defer utils.CaptureException()
		t.onReconnect(ctx)
	})

	atomic.StoreInt32(&t.needReconnect, 1)

	return nil
}

// disconnect send cancel signal, and wait onRecv and onSend goroutine's context done
func (t *TransportClient) disconnect() {
	log.Info().Int64("client_id", t.c.Id).Msg("transport client disconnect")

	// close(t.chSend)
	t.cancelRecvSend()
	atomic.StoreInt32(&t.connected, 0)
	t.wg.Wait()

	if t.ts != nil {
		t.ts.Close()
	}
}

func (t *TransportClient) StartDisconnect() {
	t.chDisconnect <- 1
}

func (t *TransportClient) SendMessage(msg proto.Message) {
	if msg == nil {
		return
	}

	if t.ts == nil {
		log.Warn().Msg("未连接到服务器")
		return
	}

	t.chSend <- msg
}

func (t *TransportClient) SetGateInfo(info *GateInfo) {
	t.gateInfo = info
}

func (t *TransportClient) SetProtocol(p string) {
	t.protocol = p
}

func (t *TransportClient) onSend(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			log.Info().Int64("client_id", t.c.Id).Msg("transport client send goroutine done...")
			return nil

		case msg := <-t.chSend:
			if atomic.LoadInt32(&t.connected) == 0 {
				log.Warn().Msg("TransportClient.onSend failed: unconnected to server")
				continue
			}

			if err := t.ts.Send(msg); err != nil {
				return fmt.Errorf("TransportClient.OnSend failed: %w", err)
			}
		}
	}
}

func (t *TransportClient) onRecv(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			log.Info().Int64("client_id", t.c.Id).Msg("transport client recv goroutine done...")
			return nil

		default:
			// be called per 100ms
			// ct := time.Now()
			// defer func() {
			// d := time.Since(ct)
			// time.Sleep(100*time.Millisecond - d)
			// }()

			if atomic.LoadInt32(&t.connected) == 0 {
				log.Warn().Msg("TransportClient.onRecv failed: unconnected to server")
				continue
			}

			if msg, h, err := t.ts.Recv(t.c.msgHandler.r); err != nil {
				if errors.Is(err, transport.ErrUnregistedMessage) {
					log.Warn().Err(err).Msg("TransportSocket Recv failed")
					continue
				}

				return fmt.Errorf("TransportClient.onRecv failed: %w", err)

			} else {
				err := h.Fn(ctx, t.ts, msg)
				if err != nil {
					return fmt.Errorf("TransportClient.onRecv failed: %w", err)
				}

				name := msg.ProtoReflect().Descriptor().Name()
				if name != "S2C_HeartBeat" {
					t.returnMsgName <- string(name)
					atomic.AddInt32(&t.unProcedMsg, 1)
					num := atomic.LoadInt32(&t.unProcedMsg)
					if num >= 90 {
						log.Warn().Int64("client_id", t.c.Id).Int32("unproc", num).Msg("return msg name ")
					}
				}
			}
		}
	}
}

func (t *TransportClient) onReconnect(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			log.Info().Int64("client_id", t.c.Id).Msg("transport client reconnect goroutine done...")
			return

		case <-t.chDisconnect:
			log.Info().Int64("client_id", t.c.Id).Msg("transport client disconnected, please rerun to start connection to server again")
			return

		default:
			func() {
				ct := time.Now()
				defer func() {
					d := time.Since(ct)
					time.Sleep(2*time.Second - d)
				}()

				// reconnect
				re := atomic.LoadInt32(&t.needReconnect)
				if re > 0 {
					t.disconnect()
					log.Info().Msg("start reconnect...")

					subCtx, subCancel := context.WithCancel(ctx)
					t.cancelRecvSend = subCancel
					err := t.connect(subCtx)
					if err != nil {
						log.Warn().Err(err).Msg("TransportClient.onReconnect failed")
					} else {
						atomic.StoreInt32(&t.needReconnect, 0)
					}
				}
			}()
		}
	}
}

func (t *TransportClient) Run(ctx *cli.Context) error {
	<-ctx.Done()
	log.Info().Int64("client_id", t.c.Id).Msg("transport client context done...")
	return nil
}

func (t *TransportClient) Exit(ctx *cli.Context) {
	if t.ts != nil {
		t.ts.Close()
	}

	// wait for onRecv and onSend context done
	t.wg.Wait()

	// wait for onReconnect context done
	t.wgRecon.Wait()
}

func (t *TransportClient) ReturnMsgName() <-chan string {
	return t.returnMsgName
}
