package client

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/east-eden/server/excel"
	"github.com/east-eden/server/logger"
	pbGlobal "github.com/east-eden/server/proto/global"
	"github.com/east-eden/server/utils"
	"github.com/spf13/cast"
	"github.com/urfave/cli/v2"
	"github.com/urfave/cli/v2/altsrc"

	"github.com/rs/zerolog"
	log "github.com/rs/zerolog/log"
)

var PingTotalNum int32
var ExecuteFuncChanNum int = 100
var ErrExecuteContextDone = errors.New("AddClientExecute failed: goroutine context done")
var ErrExecuteClientClosed = errors.New("AddClientExecute failed: cannot find execute client")

type ClientBots struct {
	app *cli.App
	sync.RWMutex

	gin           *GinServer
	mapClients    map[int64]*Client
	wg            utils.WaitGroupWrapper
	clientBotsNum int
	GateAddr      string
}

func NewClientBots() *ClientBots {
	c := &ClientBots{
		mapClients: make(map[int64]*Client),
	}

	c.app = cli.NewApp()
	c.app.Name = "client_bots"
	c.app.Flags = NewClientBotsFlags()
	c.app.Before = c.Before
	c.app.Action = c.Action
	c.app.UsageText = "client_bots [first_arg] [second_arg]"
	c.app.Authors = []*cli.Author{{Name: "dudu", Email: "hellodudu86@gmail"}}

	return c
}

func (c *ClientBots) Before(ctx *cli.Context) error {
	// relocate path
	if err := utils.RelocatePath("/server_bin", "/server"); err != nil {
		fmt.Println("relocate path failed: ", err)
		os.Exit(1)
	}

	// logger init
	logger.InitLogger("client_bots")

	// load excel entries
	excel.ReadAllEntries("config/csv/")

	ctx.Set("config_file", "config/client_bots/config.toml")
	return altsrc.InitInputSourceWithContext(c.app.Flags, altsrc.NewTomlSourceFromFlagFunc("config_file"))(ctx)
}

func (c *ClientBots) Action(ctx *cli.Context) error {
	// Initialize logging
	if err := c.initializeLogging(ctx); err != nil {
		return err
	}

	// Start HTTP server
	c.startGinServer(ctx)

	// Start connection monitoring
	c.startConnectionMonitor(ctx)

	// Start all client bots
	c.startClientBots(ctx)

	return nil
}

// initializeLogging sets up the logging configuration
func (c *ClientBots) initializeLogging(ctx *cli.Context) error {
	logLevel, err := zerolog.ParseLevel(ctx.String("log_level"))
	if err != nil {
		log.Fatal().Err(err).Send()
		return err
	}

	log.Logger = log.Level(logLevel)
	log.Info().Str("level", logLevel.String()).Msg("logging initialized")
	return nil
}

// startGinServer starts the HTTP server for debugging and monitoring
func (c *ClientBots) startGinServer(ctx *cli.Context) {
	c.gin = NewGinServer(ctx)

	c.wg.Wrap(func() {
		defer func() {
			if err := recover(); err != nil {
				stack := string(debug.Stack())
				log.Error().Msgf("gin server panic: %v, stack: %s", err, stack)
			}
			c.gin.Exit(ctx.Context)
		}()

		if err := c.gin.Main(ctx); err != nil {
			log.Warn().Err(err).Msg("gin server exited with error")
		}
	})
}

// startConnectionMonitor starts the connection status monitoring
func (c *ClientBots) startConnectionMonitor(ctx *cli.Context) {
	c.wg.Wrap(func() {
		defer utils.CaptureException()

		ticker := time.NewTicker(time.Second * 5)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				log.Info().Msg("connection monitor stopped")
				return
			case <-ticker.C:
				c.updateConnectionInfo()
			}
		}
	})
}

// updateConnectionInfo logs current connection statistics
func (c *ClientBots) updateConnectionInfo() {
	c.RLock()
	connectionCount := len(c.mapClients)
	c.RUnlock()

	log.Warn().Int("connection_num", connectionCount).Msg("client bots infos update")
}

// startClientBots initializes and starts all client bots
func (c *ClientBots) startClientBots(ctx *cli.Context) {
	c.clientBotsNum = ctx.Int("client_bots_num")
	c.GateAddr = ctx.String("gate_addr")

	log.Info().Int("count", c.clientBotsNum).Msg("starting client bots")

	for n := 0; n < c.clientBotsNum; n++ {
		clientID := int64(n)
		c.startSingleClient(ctx, clientID)

		// Stagger client startup to avoid resource contention
		time.Sleep(time.Millisecond * 10)
	}

	log.Info().Int("count", c.clientBotsNum).Msg("all client bots started")
}

// startSingleClient starts a single client bot with the given ID
func (c *ClientBots) startSingleClient(ctx *cli.Context, clientID int64) {
	// Create client configuration
	clientCtx := c.createClientContext(ctx, clientID)
	execChan := make(chan ExecuteFunc, ExecuteFuncChanNum)

	// Create and register client
	newClient := NewClient(execChan)
	c.registerClient(clientID, newClient)

	// Start client runner
	c.startClientRunner(clientCtx, clientID, newClient)

	// Start client execution sequence
	c.startClientExecution(ctx, clientID)
}

// createClientContext creates a CLI context for a specific client
func (c *ClientBots) createClientContext(ctx *cli.Context, clientID int64) *cli.Context {
	set := flag.NewFlagSet("clientbot", flag.ContinueOnError)

	// Client-specific configuration
	set.Int64("client_id", clientID, "client id")
	set.String("http_listen_addr", fmt.Sprintf(":%d", 8090+clientID), "http listen address")
	set.Bool("open_gin", false, "open gin server")

	// Inherit parent configuration
	c.inheritParentConfig(set, ctx)

	return cli.NewContext(nil, set, nil)
}

// inheritParentConfig copies configuration from parent context
func (c *ClientBots) inheritParentConfig(set *flag.FlagSet, ctx *cli.Context) {
	set.String("gate_addr", ctx.String("gate_addr"), "gate address")
	set.String("cert_path_debug", ctx.String("cert_path_debug"), "cert path debug")
	set.String("key_path_debug", ctx.String("key_path_debug"), "key path debug")
	set.String("cert_path_release", ctx.String("cert_path_release"), "cert path release")
	set.String("key_path_release", ctx.String("key_path_release"), "key path release")
	set.Bool("debug", ctx.Bool("debug"), "debug mode")
	set.String("log_level", ctx.String("log_level"), "log level")
	set.Duration("heart_beat", ctx.Duration("heart_beat"), "heart beat")
}

// registerClient adds a client to the client map
func (c *ClientBots) registerClient(clientID int64, client *Client) {
	c.Lock()
	c.mapClients[clientID] = client
	c.Unlock()
}

// startClientRunner starts the main client runner goroutine
func (c *ClientBots) startClientRunner(clientCtx *cli.Context, clientID int64, client *Client) {
	c.wg.Wrap(func() {
		defer func() {
			if err := recover(); err != nil {
				stack := string(debug.Stack())
				log.Error().Int64("client_id", clientID).
					Msgf("client runner panic: %v, stack: %s", err, stack)
			}

			// Cleanup client from map
			c.unregisterClient(clientID)
			log.Info().Int64("client_id", clientID).Msg("client runner cleanup completed")
		}()

		// Run client
		if err := client.Action(clientCtx); err != nil {
			log.Error().Int64("client_id", clientID).Err(err).Msg("client action failed")
		}

		client.Stop()
		log.Info().Int64("client_id", clientID).Msg("client exited normally")
	})
}

// startClientExecution starts the client execution sequence
func (c *ClientBots) startClientExecution(ctx *cli.Context, clientID int64) {
	c.wg.Wrap(func() {
		defer utils.CaptureException()

		executor := &clientExecutor{
			clientBots: c,
			ctx:        ctx,
			clientID:   clientID,
		}

		executor.run()
	})
}

// unregisterClient removes a client from the client map
func (c *ClientBots) unregisterClient(clientID int64) {
	c.Lock()
	delete(c.mapClients, clientID)
	c.Unlock()
}

// clientExecutor handles the execution sequence for a single client
type clientExecutor struct {
	clientBots *ClientBots
	ctx        *cli.Context
	clientID   int64
	err        error
}

// run executes the client's game sequence
func (e *clientExecutor) run() {
	// Initial setup sequence
	e.addExecute(LogonExecution)
	e.addExecute(CreatePlayerExecution)

	if e.err != nil {
		log.Error().Int64("client_id", e.clientID).Err(e.err).
			Msg("client setup failed")
		return
	}

	// Continuous game operations - 压测模式：快速连续添加物品
	for e.err == nil {
		// 连续执行多次添加操作进行压测
		e.addExecute(AddItemExecution)
		e.addExecute(AddItemExecution)
		e.addExecute(AddItemExecution)

		// 短暂休息避免过度压测
		time.Sleep(time.Millisecond * 10)
	}

	log.Info().Int64("client_id", e.clientID).Err(e.err).
		Msg("client execution stopped")
}

// addExecute adds an execution function to the client's queue
func (e *clientExecutor) addExecute(fn ExecuteFunc) {
	if e.err != nil {
		return
	}

	select {
	case <-e.ctx.Done():
		e.err = errors.New("context cancelled")
		return
	default:
	}

	e.err = e.clientBots.AddClientExecute(e.ctx.Context, e.clientID, fn)
}

func (c *ClientBots) Run(arguments []string) error {
	// app run
	if err := c.app.Run(arguments); err != nil {
		return err
	}

	return nil
}

func (c *ClientBots) Stop() {
	c.wg.Wait()
}

func (c *ClientBots) AddClientExecute(ctx context.Context, id int64, fn ExecuteFunc) error {
	select {
	case <-ctx.Done():
		return ErrExecuteContextDone
	default:
	}

	time.Sleep(time.Millisecond * 50) // 减少到50ms，提高频率

	c.RLock()
	defer c.RUnlock()

	client, ok := c.mapClients[id]
	if !ok {
		return ErrExecuteClientClosed
	}

	if len(client.chExec) >= ExecuteFuncChanNum {
		return errors.New("channel full")
	}

	client.chExec <- fn
	return nil
}

func PingExecution(ctx context.Context, c *Client) error {
	msg := &pbGlobal.C2S_Ping{
		Ping: 1,
	}

	c.transport.SendMessage(msg)

	c.WaitReturnedMsg(ctx, "S2C_Pong")
	atomic.AddInt32(&PingTotalNum, 1)
	return nil
}

// LogonExecution handles the complete client login process
func LogonExecution(ctx context.Context, c *Client) error {
	logonHandler := &ClientLogonHandler{
		client: c,
		ctx:    ctx,
	}

	return logonHandler.Execute()
}

// ClientLogonHandler encapsulates the login process logic
type ClientLogonHandler struct {
	client   *Client
	ctx      context.Context
	gateInfo GateInfo
}

// Execute performs the complete login sequence
func (h *ClientLogonHandler) Execute() error {
	log.Info().Int64("client_id", h.client.Id).Msg("starting client login process")

	// Step 1: Prepare gate connection info
	if err := h.prepareGateInfo(); err != nil {
		return fmt.Errorf("failed to prepare gate info: %w", err)
	}

	// Step 2: Configure transport client
	if err := h.configureTransport(); err != nil {
		return fmt.Errorf("failed to configure transport: %w", err)
	}

	// Step 3: Establish connection to gate
	if err := h.establishConnection(); err != nil {
		return fmt.Errorf("failed to establish connection: %w", err)
	}

	// Step 4: Wait for login response
	if err := h.waitForLoginResponse(); err != nil {
		return fmt.Errorf("failed to receive login response: %w", err)
	}

	log.Info().Int64("client_id", h.client.Id).Msg("client login completed successfully")
	return nil
}

// prepareGateInfo creates and validates gate connection information
func (h *ClientLogonHandler) prepareGateInfo() error {
	h.gateInfo = GateInfo{
		UserID:        cast.ToString(h.client.Id),
		AccountID:     h.client.Id,
		UserName:      fmt.Sprintf("bot%d", h.client.Id),
		PublicTcpAddr: h.client.GateAddr,
	}

	// Validate gate address
	if len(h.gateInfo.PublicTcpAddr) == 0 {
		return errors.New("invalid gate public address: address is empty")
	}

	log.Debug().Int64("client_id", h.client.Id).
		Interface("gate_info", h.gateInfo).
		Msg("gate connection info prepared")

	return nil
}

// configureTransport sets up the transport client with gate info and protocol
func (h *ClientLogonHandler) configureTransport() error {
	h.client.transport.SetGateInfo(&h.gateInfo)
	h.client.transport.SetProtocol("tcp")

	log.Debug().Int64("client_id", h.client.Id).
		Str("protocol", "tcp").
		Msg("transport client configured")

	return nil
}

// establishConnection connects to the gate server
func (h *ClientLogonHandler) establishConnection() error {
	log.Info().Int64("client_id", h.client.Id).
		Str("gate_addr", h.gateInfo.PublicTcpAddr).
		Msg("establishing connection to gate server")

	if err := h.client.transport.StartConnect(h.ctx); err != nil {
		log.Error().Int64("client_id", h.client.Id).
			Err(err).Msg("failed to connect to gate server")
		return err
	}

	// Wait for connection to be fully established
	h.waitForConnectionStabilization()

	log.Info().Int64("client_id", h.client.Id).Msg("connection to gate server established")
	return nil
}

// waitForConnectionStabilization waits for the connection to stabilize
func (h *ClientLogonHandler) waitForConnectionStabilization() {
	const stabilizationDelay = time.Millisecond * 1000

	log.Debug().Int64("client_id", h.client.Id).
		Dur("delay", stabilizationDelay).
		Msg("waiting for connection stabilization")

	time.Sleep(stabilizationDelay)
}

// waitForLoginResponse waits for the server's login response
func (h *ClientLogonHandler) waitForLoginResponse() error {
	const expectedResponse = "S2C_AccountLogon"

	log.Info().Int64("client_id", h.client.Id).
		Str("expected_message", expectedResponse).
		Msg("waiting for login response from server")

	// TransportClient automatically sends handshake and login messages
	// We just need to wait for the response
	success := h.client.WaitReturnedMsg(h.ctx, expectedResponse)
	if !success {
		log.Error().Int64("client_id", h.client.Id).
			Str("expected_message", expectedResponse).
			Msg("timeout waiting for login response")
		return errors.New("timeout waiting for login response")
	}

	log.Info().Int64("client_id", h.client.Id).
		Str("received_message", expectedResponse).
		Msg("login response received successfully")

	return nil
}

// CreatePlayerExecution handles the player creation process
func CreatePlayerExecution(ctx context.Context, c *Client) error {
	handler := &PlayerCreationHandler{
		client: c,
		ctx:    ctx,
	}

	return handler.Execute()
}

// PlayerCreationHandler encapsulates the player creation logic
type PlayerCreationHandler struct {
	client *Client
	ctx    context.Context
}

// Execute performs the complete player creation sequence
func (h *PlayerCreationHandler) Execute() error {
	log.Info().Int64("client_id", h.client.Id).Msg("starting player creation process")

	// Step 1: Create player creation message
	message := h.createPlayerMessage()

	// Step 2: Send creation request
	if err := h.sendCreationRequest(message); err != nil {
		return fmt.Errorf("failed to send player creation request: %w", err)
	}

	// Step 3: Wait for creation response
	if err := h.waitForCreationResponse(); err != nil {
		return fmt.Errorf("failed to receive player creation response: %w", err)
	}

	log.Info().Int64("client_id", h.client.Id).Msg("player creation completed successfully")
	return nil
}

// createPlayerMessage creates the player creation message
func (h *PlayerCreationHandler) createPlayerMessage() *pbGlobal.C2S_CreatePlayer {
	playerName := fmt.Sprintf("bot%d", h.client.Id)

	message := &pbGlobal.C2S_CreatePlayer{
		Name: playerName,
	}

	log.Debug().Int64("client_id", h.client.Id).
		Str("player_name", playerName).
		Msg("player creation message prepared")

	return message
}

// sendCreationRequest sends the player creation request to the server
func (h *PlayerCreationHandler) sendCreationRequest(message *pbGlobal.C2S_CreatePlayer) error {
	log.Debug().Int64("client_id", h.client.Id).
		Str("message_type", "C2S_CreatePlayer").
		Msg("sending player creation request")

	h.client.transport.SendMessage(message)
	return nil
}

// waitForCreationResponse waits for the server's player creation response
func (h *PlayerCreationHandler) waitForCreationResponse() error {
	const expectedResponse = "S2C_CreatePlayer"

	log.Info().Int64("client_id", h.client.Id).
		Str("expected_message", expectedResponse).
		Msg("waiting for player creation response")

	success := h.client.WaitReturnedMsg(h.ctx, expectedResponse)
	if !success {
		log.Error().Int64("client_id", h.client.Id).
			Str("expected_message", expectedResponse).
			Msg("timeout waiting for player creation response")
		return errors.New("timeout waiting for player creation response")
	}

	log.Info().Int64("client_id", h.client.Id).
		Str("received_message", expectedResponse).
		Msg("player creation response received successfully")

	return nil
}

// AddItemExecution handles the GM command to add items
func AddItemExecution(ctx context.Context, c *Client) error {
	handler := &GMCommandHandler{
		client: c,
		ctx:    ctx,
	}

	return handler.ExecuteAddItem()
}

// GMCommandHandler encapsulates GM command execution logic
type GMCommandHandler struct {
	client *Client
	ctx    context.Context
}

// ExecuteAddItem executes the add item GM command with stress testing
func (h *GMCommandHandler) ExecuteAddItem() error {
	// 随机选择物品类型和数量进行压测
	itemTypes := []int{6, 7, 8, 9, 10, 11, 12} // 多种物品类型
	quantities := []int{1, 5, 10, 20, 50}      // 不同数量

	itemType := itemTypes[h.client.Id%int64(len(itemTypes))]
	quantity := quantities[h.client.Id%int64(len(quantities))]

	command := fmt.Sprintf("gm item add %d %d", itemType, quantity)

	log.Debug().Int64("client_id", h.client.Id). // 改为Debug减少日志量
							Str("command", command).
							Msg("executing stress test GM add item command")

	// Step 1: Create GM command message
	message := h.createGMCommandMessage(command)

	// Step 2: Send GM command
	if err := h.sendGMCommand(message); err != nil {
		return fmt.Errorf("failed to send GM command: %w", err)
	}

	// Step 3: Wait for command response (减少等待时间)
	if err := h.waitForGMResponseFast(); err != nil {
		return fmt.Errorf("failed to receive GM command response: %w", err)
	}

	log.Debug().Int64("client_id", h.client.Id). // 改为Debug减少日志量
							Str("command", command).
							Msg("stress test GM add item command completed")

	return nil
}

// createGMCommandMessage creates a GM command message
func (h *GMCommandHandler) createGMCommandMessage(command string) *pbGlobal.C2S_GmCmd {
	message := &pbGlobal.C2S_GmCmd{
		Cmd: command,
	}

	log.Debug().Int64("client_id", h.client.Id).
		Str("command", command).
		Msg("GM command message prepared")

	return message
}

// sendGMCommand sends the GM command to the server
func (h *GMCommandHandler) sendGMCommand(message *pbGlobal.C2S_GmCmd) error {
	log.Debug().Int64("client_id", h.client.Id).
		Str("message_type", "C2S_GmCmd").
		Msg("sending GM command")

	h.client.transport.SendMessage(message)
	return nil
}

// waitForGMResponse waits for the server's GM command response
func (h *GMCommandHandler) waitForGMResponse() error {
	const expectedResponse = "S2C_ServerConsole"

	log.Debug().Int64("client_id", h.client.Id).
		Str("expected_message", expectedResponse).
		Msg("waiting for GM command response")

	success := h.client.WaitReturnedMsg(h.ctx, expectedResponse)
	if !success {
		log.Warn().Int64("client_id", h.client.Id).
			Str("expected_message", expectedResponse).
			Msg("timeout waiting for GM command response")
		return errors.New("timeout waiting for GM command response")
	}

	log.Debug().Int64("client_id", h.client.Id).
		Str("received_message", expectedResponse).
		Msg("GM command response received successfully")

	return nil
}

// waitForGMResponseFast waits for GM response with shorter timeout for stress testing
func (h *GMCommandHandler) waitForGMResponseFast() error {
	const expectedResponse = "S2C_ServerConsole"

	// 使用更短的超时时间进行压测
	ctx, cancel := context.WithTimeout(h.ctx, time.Millisecond*200)
	defer cancel()

	success := h.client.WaitReturnedMsg(ctx, expectedResponse)
	if !success {
		// 压测时不记录超时警告，避免日志过多
		return errors.New("timeout waiting for GM command response")
	}

	return nil
}
