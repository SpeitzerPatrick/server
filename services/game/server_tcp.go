package game

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/east-eden/server/services/game/player"
	"github.com/east-eden/server/transport"
	"github.com/east-eden/server/utils"
	"github.com/panjf2000/ants/v2"
	log "github.com/rs/zerolog/log"
	"github.com/urfave/cli/v2"
)

var (
	TcpRecvInterval = time.Millisecond * 100 // tcp recv interval per connection
)

type TcpServer struct {
	tr   transport.Transport
	reg  transport.Register
	g    *Game
	wg   sync.WaitGroup
	pool *ants.Pool
}

func NewTcpServer(ctx *cli.Context, g *Game) *TcpServer {
	maxAccount := ctx.Int("account_connect_max")

	s := &TcpServer{
		g:   g,
		reg: g.msgRegister.r,
	}

	var err error
	s.pool, err = ants.NewPool(maxAccount, ants.WithExpiryDuration(10*time.Second))
	if !utils.ErrCheck(err, "NewPool failed when NewTcpServer", maxAccount) {
		return nil
	}

	err = s.serve(ctx)
	if !utils.ErrCheck(err, "serve failed when NewTcpServer", maxAccount) {
		return nil
	}

	return s
}

func (s *TcpServer) serve(ctx *cli.Context) error {
	s.tr = transport.NewTransport("tcp")

	s.tr.Init(
		transport.Timeout(transport.DefaultServeTimeout),
	)

	go func() {
		defer utils.CaptureException()

		err := s.tr.ListenAndServe(ctx.Context, ctx.String("tcp_listen_addr"), s)
		if err != nil {
			log.Warn().Err(err).Msg("tcp server ListenAndServe return with error")
			os.Exit(1)
		}
	}()

	log.Info().
		Str("addr", ctx.String("tcp_listen_addr")).
		Msg("tcp server serve at address")

	return nil
}

func (s *TcpServer) Run(ctx context.Context) error {
	<-ctx.Done()
	log.Info().Msg("tcp server context done...")
	return nil
}

func (s *TcpServer) Exit() {
	s.wg.Wait()
	log.Info().Msg("tcp server exit...")
}

// SocketHandler encapsulates socket handling logic
type SocketHandler struct {
	server *TcpServer
	sock   transport.Socket
	ctx    context.Context
	cancel context.CancelFunc
	stats  *SocketStats
}

// SocketStats tracks socket handling statistics
type SocketStats struct {
	MessagesReceived int64
	MessagesHandled  int64
	ErrorsCount      int64
	StartTime        time.Time
	LastActivity     time.Time
}

// NewSocketHandler creates a new socket handler
func NewSocketHandler(server *TcpServer, sock transport.Socket, ctx context.Context) *SocketHandler {
	subCtx, cancel := context.WithCancel(ctx)
	return &SocketHandler{
		server: server,
		sock:   sock,
		ctx:    subCtx,
		cancel: cancel,
		stats: &SocketStats{
			StartTime:    time.Now(),
			LastActivity: time.Now(),
		},
	}
}

// HandleSocket processes socket connections with improved error handling and logging
func (s *TcpServer) HandleSocket(ctx context.Context, sock transport.Socket) {
	handler := NewSocketHandler(s, sock, ctx)

	s.wg.Add(1)
	err := s.pool.Submit(func() {
		defer handler.cleanup()
		handler.processMessages()
	})

	if err != nil {
		log.Error().Err(err).
			Str("remote_addr", sock.Remote()).
			Msg("failed to submit socket handler to pool")
		handler.cleanup()
	}
}

// cleanup handles resource cleanup and logging
func (h *SocketHandler) cleanup() {
	defer func() {
		if err := recover(); err != nil {
			stack := string(debug.Stack())
			log.Error().
				Interface("panic", err).
				Str("stack", stack).
				Str("remote_addr", h.sock.Remote()).
				Msg("socket handler panic recovered")
		}

		h.sock.Close()
		h.cancel()
		h.server.wg.Done()
	}()

	duration := time.Since(h.stats.StartTime)
	log.Debug().
		Str("remote_addr", h.sock.Remote()).
		Int64("messages_received", h.stats.MessagesReceived).
		Int64("messages_handled", h.stats.MessagesHandled).
		Int64("errors_count", h.stats.ErrorsCount).
		Dur("duration", duration).
		Msg("socket handler cleanup completed")
}

// processMessages handles the main message processing loop
func (h *SocketHandler) processMessages() {
	log.Info().
		Str("remote_addr", h.sock.Remote()).
		Msg("socket handler started")

	for {
		if !h.shouldContinueProcessing() {
			return
		}

		if err := h.processNextMessage(); err != nil {
			if h.isConnectionClosed(err) {
				log.Debug().
					Str("remote_addr", h.sock.Remote()).
					Msg("socket connection closed")
				return
			}

			if h.shouldDisconnect(err) {
				log.Info().
					Str("remote_addr", h.sock.Remote()).
					Msg("socket disconnecting due to account disconnect")
				return
			}

			h.stats.ErrorsCount++
			log.Warn().
				Err(err).
				Str("remote_addr", h.sock.Remote()).
				Int64("error_count", h.stats.ErrorsCount).
				Msg("socket message processing error")
		}

		h.throttleProcessing()
	}
}

// shouldContinueProcessing checks if processing should continue
func (h *SocketHandler) shouldContinueProcessing() bool {
	select {
	case <-h.ctx.Done():
		log.Debug().
			Str("remote_addr", h.sock.Remote()).
			Msg("socket handler context cancelled")
		return false
	default:
		return true
	}
}

// processNextMessage processes a single message
func (h *SocketHandler) processNextMessage() error {
	startTime := time.Now()

	msg, handler, err := h.sock.Recv(h.server.reg)
	if err != nil {
		return err
	}

	h.stats.MessagesReceived++
	h.stats.LastActivity = time.Now()

	msgName := string(msg.ProtoReflect().Descriptor().Name())

	log.Debug().
		Str("remote_addr", h.sock.Remote()).
		Str("message_type", msgName).
		Int64("message_count", h.stats.MessagesReceived).
		Msg("processing message")

	if err := handler.Fn(h.ctx, h.sock, msg); err != nil {
		return fmt.Errorf("message handler failed for %s: %w", msgName, err)
	}

	h.stats.MessagesHandled++
	processingTime := time.Since(startTime)

	if processingTime > time.Millisecond*100 {
		log.Warn().
			Str("remote_addr", h.sock.Remote()).
			Str("message_type", msgName).
			Dur("processing_time", processingTime).
			Msg("slow message processing detected")
	}

	return nil
}

// isConnectionClosed checks if the error indicates a closed connection
func (h *SocketHandler) isConnectionClosed(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed)
}

// shouldDisconnect checks if the error requires disconnection
func (h *SocketHandler) shouldDisconnect(err error) bool {
	return errors.Is(err, player.ErrAccountDisconnect)
}

// throttleProcessing implements processing throttling
func (h *SocketHandler) throttleProcessing() {
	// Adaptive throttling based on message processing rate
	if h.stats.MessagesReceived > 0 {
		avgProcessingTime := time.Since(h.stats.StartTime) / time.Duration(h.stats.MessagesReceived)
		if avgProcessingTime < TcpRecvInterval {
			time.Sleep(TcpRecvInterval - avgProcessingTime)
		}
	} else {
		time.Sleep(TcpRecvInterval)
	}
}
