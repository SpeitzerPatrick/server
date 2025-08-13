package client

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/east-eden/server/excel"
	"github.com/east-eden/server/logger"
	"github.com/east-eden/server/utils"
	"github.com/rs/zerolog"
	log "github.com/rs/zerolog/log"
	"github.com/urfave/cli/v2"
	"github.com/urfave/cli/v2/altsrc"
	"google.golang.org/protobuf/proto"
)

type ExecuteFunc func(context.Context, *Client) error

type Client struct {
	app *cli.App
	Id  int64
	sync.RWMutex

	player     *Player
	gin        *GinServer
	transport  *TransportClient
	msgHandler *MsgHandler
	cmder      *Commander
	prompt     *PromptUI
	chExec     chan ExecuteFunc
	exitFunc   func(error) // Exit handler function

	GateAddr string
	wg       utils.WaitGroupWrapper
}

func NewClient(ch chan ExecuteFunc) *Client {
	c := &Client{
		chExec: ch,
	}

	if ch == nil {
		c.chExec = make(chan ExecuteFunc, ExecuteFuncChanNum)
	}

	c.app = cli.NewApp()
	c.app.Name = "client"
	c.app.Flags = NewFlags()
	c.app.Before = c.Before
	c.app.Action = c.Action
	c.app.UsageText = "client [first_arg] [second_arg]"
	c.app.Authors = []*cli.Author{{Name: "dudu", Email: "hellodudu86@gmail"}}

	return c
}

func (c *Client) Before(ctx *cli.Context) error {
	// relocate path
	if err := utils.RelocatePath("/server_bin", "/server"); err != nil {
		fmt.Println("relocate path failed: ", err)
		os.Exit(1)
	}

	// logger init
	logger.InitLogger("game")

	// load excel entries
	excel.ReadAllEntries("config/csv/")

	ctx.Set("config_file", "config/client/config.toml")
	return altsrc.InitInputSourceWithContext(c.app.Flags, altsrc.NewTomlSourceFromFlagFunc("config_file"))(ctx)
}

func (c *Client) Action(ctx *cli.Context) error {
	// Initialize client configuration and logging
	if err := c.initialize(ctx); err != nil {
		return err
	}

	// Setup exit handling
	exitCh := c.setupExitHandler()

	// Initialize all client components
	c.initializeComponents(ctx)

	// Start all client services
	c.startServices(ctx, exitCh)

	// Wait for exit signal
	return <-exitCh
}

// initialize sets up basic client configuration and logging
func (c *Client) initialize(ctx *cli.Context) error {
	// Configure logging
	logLevel, err := zerolog.ParseLevel(ctx.String("log_level"))
	if err != nil {
		log.Fatal().Err(err).Send()
		return err
	}
	log.Logger = log.Level(logLevel)

	// Set client configuration
	c.Id = ctx.Int64("client_id")
	c.GateAddr = ctx.String("gate_addr")

	log.Info().Int64("client_id", c.Id).Str("gate_addr", c.GateAddr).
		Msg("client initialized")

	return nil
}

// setupExitHandler creates the exit channel and handler function
func (c *Client) setupExitHandler() chan error {
	exitCh := make(chan error, 1)
	var once sync.Once

	c.exitFunc = func(err error) {
		once.Do(func() {
			if err != nil {
				log.Error().Err(err).Int64("client_id", c.Id).
					Msg("client exiting with error")
			} else {
				log.Info().Int64("client_id", c.Id).Msg("client exiting normally")
			}
			exitCh <- err
		})
	}

	return exitCh
}

// initializeComponents creates all client components
func (c *Client) initializeComponents(ctx *cli.Context) {
	log.Debug().Int64("client_id", c.Id).Msg("initializing client components")

	c.cmder = NewCommander(c)
	c.prompt = NewPromptUI(ctx, c)
	c.transport = NewTransportClient(ctx, c)
	c.msgHandler = NewMsgHandler(ctx, c)
	c.player = NewPlayer(ctx, c)

	// Initialize optional HTTP server
	if ctx.Bool("open_gin") {
		c.gin = NewGinServer(ctx)
		log.Debug().Int64("client_id", c.Id).Msg("gin server initialized")
	}

	log.Info().Int64("client_id", c.Id).Msg("all client components initialized")
}

// startServices starts all client services in separate goroutines
func (c *Client) startServices(ctx *cli.Context, exitCh chan error) {
	c.startPromptUI(ctx)
	c.startTransportClient(ctx)
	c.startGinServer(ctx)
	c.startExecutor(ctx)
}

func (c *Client) Run(arguments []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// app run
	if err := c.app.RunContext(ctx, arguments); err != nil {
		return err
	}

	return nil
}

func (c *Client) Execute(ctx *cli.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil

		case fn, ok := <-c.chExec:
			if !ok {
				log.Warn().Int64("id", c.Id).Msg("client execute channel read failed")
			} else {
				err := fn(ctx.Context, c)
				if err != nil {
					return fmt.Errorf("Client.Execute failed: %w", err)
				}
			}
		}
	}
}

// startPromptUI starts the prompt UI service
func (c *Client) startPromptUI(ctx *cli.Context) {
	c.wg.Wrap(func() {
		defer utils.CaptureException()

		log.Debug().Int64("client_id", c.Id).Msg("starting prompt UI")
		if err := c.prompt.Run(ctx); err != nil {
			log.Warn().Err(err).Int64("client_id", c.Id).Msg("prompt UI exited with error")
		}
	})
}

// startTransportClient starts the transport client service
func (c *Client) startTransportClient(ctx *cli.Context) {
	c.wg.Wrap(func() {
		defer func() {
			utils.CaptureException()
			c.transport.Exit(ctx)
			log.Debug().Int64("client_id", c.Id).Msg("transport client cleanup completed")
		}()

		log.Debug().Int64("client_id", c.Id).Msg("starting transport client")
		if err := c.transport.Run(ctx); err != nil {
			log.Error().Err(err).Int64("client_id", c.Id).Msg("transport client failed")
		}
	})
}

// startGinServer starts the HTTP server if enabled
func (c *Client) startGinServer(ctx *cli.Context) {
	if !ctx.Bool("open_gin") || c.gin == nil {
		return
	}

	c.wg.Wrap(func() {
		defer func() {
			if err := recover(); err != nil {
				stack := string(debug.Stack())
				log.Error().Int64("client_id", c.Id).
					Msgf("gin server panic: %v, stack: %s", err, stack)
			}

			c.gin.Exit(ctx.Context)
			log.Debug().Int64("client_id", c.Id).Msg("gin server cleanup completed")
		}()

		log.Debug().Int64("client_id", c.Id).Msg("starting gin server")
		if err := c.gin.Main(ctx); err != nil {
			log.Error().Err(err).Int64("client_id", c.Id).Msg("gin server failed")
			c.exitFunc(err)
		}
	})
}

// startExecutor starts the main client executor
func (c *Client) startExecutor(ctx *cli.Context) {
	c.wg.Wrap(func() {
		defer utils.CaptureException()

		log.Debug().Int64("client_id", c.Id).Msg("starting client executor")
		if err := c.Execute(ctx); err != nil {
			log.Error().Err(err).Int64("client_id", c.Id).Msg("client executor failed")
			c.exitFunc(err)
		} else {
			c.exitFunc(nil)
		}
	})
}

func (c *Client) Stop() {
	log.Info().Int64("client_id", c.Id).Msg("stopping client")
	c.wg.Wait()
	log.Info().Int64("client_id", c.Id).Msg("client stopped")
}

func (c *Client) SendMessage(msg proto.Message) {
	c.transport.SendMessage(msg)
}

func (c *Client) WaitReturnedMsg(ctx context.Context, waitMsgNames string) bool {
	// no need to wait return message
	if len(waitMsgNames) == 0 {
		return true
	}

	// default wait time - increased for better reliability
	tm := time.NewTimer(time.Second * 10)
	for {
		select {
		case <-ctx.Done():
			return false
		case name := <-c.transport.ReturnMsgName():
			atomic.AddInt32(&c.transport.unProcedMsg, -1)
			names := strings.Split(waitMsgNames, ",")
			for _, n := range names {
				if n == name {
					log.Info().Int64("id", c.Id).Str("message_name", name).Msg("client wait for returned message")
					return true
				}
			}

		case <-tm.C:
			log.Warn().Int64("id", c.Id).Str("message_name", waitMsgNames).Msg("client wait for returned message timeout")
			return false
		}
	}
}
