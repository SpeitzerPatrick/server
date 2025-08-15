package game

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/east-eden/server/define"
	"github.com/east-eden/server/services/game/player"
	"github.com/east-eden/server/services/game/prom"
	"github.com/east-eden/server/store"
	"github.com/east-eden/server/transport"
	"github.com/east-eden/server/utils"
	"github.com/east-eden/server/utils/cache"
	"github.com/hellodudu/task"
	log "github.com/rs/zerolog/log"
	"google.golang.org/protobuf/proto"

	// "github.com/sasha-s/go-deadlock"
	"github.com/urfave/cli/v2"
)

var (
	CacheCleanupInterval  = 1 * time.Minute  // cache cleanup interval
	UserCacheExpire       = 10 * time.Minute // user cache缓存10分钟
	AccountCacheExpire    = 1 * time.Minute  // 账号cache缓存10分钟
	PlayerInfoCacheExpire = time.Hour        // 玩家简易信息cache缓存1小时

	ErrAccountHasNoPlayer    = errors.New("account has no player")
	ErrAccountNotFound       = errors.New("account not found")
	ErrAccountTaskNotRunning = errors.New("account task not running")
	ErrPlayerInfoNotFound    = errors.New("player info not found")
	ErrPlayerLoadFailed      = errors.New("player load failed")
)

type AccountManagerFace interface {
}

type AccountManager struct {
	cacheAccounts    *cache.Cache
	cacheUsers       *cache.Cache
	cachePlayerInfos *cache.Cache
	mapSocks         map[transport.Socket]int64 // socket->accountId

	g  *Game
	wg utils.WaitGroupWrapper

	accountConnectMax int

	userPool       sync.Pool
	playerPool     sync.Pool
	accountPool    sync.Pool
	playerInfoPool sync.Pool

	sync.RWMutex
}

func NewAccountManager(ctx *cli.Context, g *Game) *AccountManager {
	am := &AccountManager{
		g:                 g,
		cacheAccounts:     cache.New(AccountCacheExpire, CacheCleanupInterval),
		cacheUsers:        cache.New(UserCacheExpire, CacheCleanupInterval),
		cachePlayerInfos:  cache.New(PlayerInfoCacheExpire, CacheCleanupInterval),
		mapSocks:          make(map[transport.Socket]int64),
		accountConnectMax: ctx.Int("account_connect_max"),
	}

	// heart beat timeout
	player.AccountTaskTimeout = ctx.Duration("heart_beat_timeout")

	// user pool
	am.userPool.New = NewUser

	// user cache evicted
	am.cacheUsers.OnEvicted(func(k, v any) {
		log.Info().Interface("key", k).Interface("value", v).Msg("user cache evicted")
		am.userPool.Put(v)
	})

	// player info cache evicted
	am.cachePlayerInfos.OnEvicted(func(k, v any) {
		log.Info().Interface("key", k).Interface("value", v).Msg("player info cache evicted")
		am.playerInfoPool.Put(v)
	})

	// 账号缓存删除时处理
	am.cacheAccounts.OnEvicted(func(k, v any) {
		acct := v.(*player.Account)

		event := log.Info().Caller().Int64("account_id", acct.Id)
		if acct.GetSock() != nil {
			event = event.Str("sock_local", acct.GetSock().Local()).
				Str("sock_remote", acct.GetSock().Remote())
		}
		event.Msg("account cache evicted")

		acct.TaskStop()
		if acct.GetPlayer() != nil {
			acct.GetPlayer().Destroy()
			am.playerPool.Put(acct.GetPlayer())
		}
		am.accountPool.Put(acct)
	})

	am.playerPool.New = player.NewPlayer
	am.accountPool.New = player.NewAccount
	am.playerInfoPool.New = player.NewPlayerInfo

	// add store info
	store.GetStore().AddStoreInfo(define.StoreType_User, "user", "_id")
	store.GetStore().AddStoreInfo(define.StoreType_Account, "account", "_id")
	store.GetStore().AddStoreInfo(define.StoreType_Player, "player", "_id")
	store.GetStore().AddStoreInfo(define.StoreType_Item, "player_item", "_id")
	store.GetStore().AddStoreInfo(define.StoreType_Hero, "player_hero", "_id")
	store.GetStore().AddStoreInfo(define.StoreType_Token, "player_token", "_id")
	store.GetStore().AddStoreInfo(define.StoreType_Fragment, "player_fragment", "_id")
	store.GetStore().AddStoreInfo(define.StoreType_Collection, "player_collection", "_id")

	// migrate user table
	if err := store.GetStore().MigrateDbTable("user", "account_id", "player_id"); err != nil {
		log.Fatal().Err(err).Msg("migrate collection user failed")
	}

	// migrate account table
	if err := store.GetStore().MigrateDbTable("account", "user_id"); err != nil {
		log.Fatal().Err(err).Msg("migrate collection account failed")
	}

	// migrate player table
	if err := store.GetStore().MigrateDbTable("player", "account_id"); err != nil {
		log.Fatal().Err(err).Msg("migrate collection player failed")
	}

	// migrate item table
	if err := store.GetStore().MigrateDbTable("player_item", "owner_id"); err != nil {
		log.Fatal().Err(err).Msg("migrate collection player_item failed")
	}

	// migrate hero table
	if err := store.GetStore().MigrateDbTable("player_hero", "owner_id"); err != nil {
		log.Fatal().Err(err).Msg("migrate collection player_hero failed")
	}

	// migrate hero table
	if err := store.GetStore().MigrateDbTable("player_token", "owner_id"); err != nil {
		log.Fatal().Err(err).Msg("migrate collection player_token failed")
	}

	// migrate fragment table
	if err := store.GetStore().MigrateDbTable("player_fragment", "owner_id"); err != nil {
		log.Fatal().Err(err).Msg("migrate collection player_fragment failed")
	}

	// migrate collection table
	if err := store.GetStore().MigrateDbTable("player_collection", "type_id", "owner_id"); err != nil {
		log.Fatal().Err(err).Msg("migrate collection player_collection failed")
	}

	log.Info().Msg("AccountManager init ok ...")
	return am
}

// todo get by userId???
// getUser 方法用于根据用户ID获取用户信息
// 如果缓存中存在用户信息，则直接返回；否则从数据库中获取
// 如果数据库中也不存在，则创建一个新用户并保存到数据库
// 参数:
//   - userId: 用户ID
//
// 返回值:
//   - *User: 用户对象指针
//   - error: 错误信息
func (am *AccountManager) getUser(userId int64) (*User, error) {
	// 从缓存中尝试获取用户信息
	u, ok := am.cacheUsers.Get(userId)
	if ok {
		return u.(*User), nil
	}

	// 从对象池中获取一个User对象
	u = am.userPool.Get()
	// 从数据库中查询用户信息
	err := store.GetStore().FindOne(context.Background(), define.StoreType_User, userId, u)
	if err == nil {
		// 查询成功，将用户信息存入缓存并返回
		am.cacheUsers.Set(userId, u, UserCacheExpire)
		return u.(*User), nil
	}

	// 如果错误类型为"无结果"，说明用户不存在，需要创建新用户
	if errors.Is(err, store.ErrNoResult) {
		// 生成新的账户ID
		accountId, err := utils.NextID(define.SnowFlake_Account)
		if err != nil {
			// 生成失败，将对象放回对象池并返回错误
			am.userPool.Put(u)
			return nil, err
		}

		// 设置用户信息
		user := u.(*User)
		user.UserID = userId
		user.AccountID = accountId

		// 将新用户信息保存到数据库
		err = store.GetStore().UpdateOne(context.Background(), define.StoreType_User, user.UserID, user, true)
		if !utils.ErrCheck(err, "UpdateOne failed when AccountManager.getUser", user) {
			// 保存失败，将对象放回对象池并返回错误
			am.userPool.Put(user)
			return nil, err
		}

		// 保存成功，将用户信息存入缓存并返回
		am.cacheUsers.Set(userId, user, UserCacheExpire)
		return user, nil
	}

	// 其他错误情况，将对象放回对象池并返回错误
	am.userPool.Put(u)
	return nil, err
}

func (am *AccountManager) Main(ctx context.Context) error {
	exitCh := make(chan error)
	var once sync.Once
	exitFunc := func(err error) {
		once.Do(func() {
			if err != nil {
				log.Fatal().Err(err).Msg("AccountManager Main() failed")
			}
			exitCh <- err
		})
	}

	am.wg.Wrap(func() {
		defer utils.CaptureException()
		exitFunc(am.Run(ctx))
	})

	return <-exitCh
}

func (am *AccountManager) Exit() {
	am.wg.Wait()
	log.Info().Msg("account manager exit...")
}

func (am *AccountManager) handleLoadPlayer(ctx context.Context, p ...any) error {
	acct := p[0].(*player.Account)

	load := func(acct *player.Account) error {
		ids := acct.GetPlayerIDs()
		if len(ids) < 1 {
			return ErrAccountHasNoPlayer
		}

		pl := am.playerPool.Get().(*player.Player)
		pl.Init(ids[0])
		pl.SetAccount(acct)
		err := store.GetStore().FindOne(context.Background(), define.StoreType_Player, ids[0], pl)
		if errors.Is(err, store.ErrNoResult) {
			acct.PlayerIDs = acct.PlayerIDs[:0]
			am.playerPool.Put(pl)
			return ErrAccountHasNoPlayer
		}

		if !utils.ErrCheck(err, "load player object failed", ids[0]) {
			am.playerPool.Put(pl)
			return err
		}

		// 加载玩家其他数据
		err = pl.AfterLoad()
		if !utils.ErrCheck(err, "player.AfterLoad failed", ids[0]) {
			am.playerPool.Put(pl)
			return fmt.Errorf("%w: %s", ErrPlayerLoadFailed, err.Error())
		}

		acct.SetPlayer(pl)
		return nil
	}

	// 加载玩家
	return load(acct)
}

// kick all cache
func (am *AccountManager) KickAllCache() {
	am.cacheUsers.DeleteAll()
	am.cacheAccounts.DeleteAll()
	am.cachePlayerInfos.DeleteAll()
	store.GetStore().Flush()
}

// 踢掉account对象
func (am *AccountManager) KickAccount(ctx context.Context, acctId int64, gameId int32) error {
	if acctId == -1 {
		return nil
	}

	// 踢掉本服account
	if int16(gameId) == am.g.ID {
		am.cacheAccounts.Delete(acctId)
		store.GetStore().Flush()
		return nil

	} else {
		// game节点不存在的话不用发送rpc
		nodeId := fmt.Sprintf("game-%d", gameId)
		srvs, err := am.g.mi.srv.Server().Options().Registry.GetService("game")
		if err != nil {
			return nil
		}

		hit := false
		for _, srv := range srvs {
			for _, node := range srv.Nodes {
				if node.Id == nodeId {
					hit = true
					break
				}
			}
		}

		if !hit {
			return nil
		}

		// 发送rpc踢掉其他服account
		rs, err := am.g.rpcHandler.CallKickAccountOffline(acctId, gameId)
		if !utils.ErrCheck(err, "kick account offline failed", acctId, gameId, rs) {
			return err
		}

		// rpc调用成功
		if rs.GetAccountId() == acctId {
			return nil
		}

		return errors.New("kick account invalid error")
	}
}

func (am *AccountManager) newAccount(ctx context.Context, userId int64, accountId int64, accountName string, sock transport.Socket) (*player.Account, error) {
	// check max connections
	am.RLock()
	socksNum := len(am.mapSocks)
	am.RUnlock()
	if socksNum >= am.accountConnectMax {
		return nil, errors.New("AccountManager.addAccount failed: Reach game server's max account connect num")
	}

	// init new account
	acct := am.accountPool.Get().(*player.Account)
	acct.Init()
	acct.SetRpcCaller(am.g.rpcHandler)

	// load account info from store
	err := store.GetStore().FindOne(context.Background(), define.StoreType_Account, accountId, acct)
	if err != nil && !errors.Is(err, store.ErrNoResult) {
		return nil, fmt.Errorf("AccountManager.addAccount failed: %w", err)
	}

	// 如果account的上次登陆game节点不是此节点，则发rpc提掉上一个登陆节点的account
	if acct.GameId != -1 && acct.GameId != am.g.ID {
		err := am.KickAccount(ctx, acct.Id, int32(acct.GameId))
		if !utils.ErrCheck(err, "kick account failed", acct.Id, acct.GameId, am.g.ID) {
			return nil, err
		}
	}

	if errors.Is(err, store.ErrNoResult) {
		// 账号首次登陆
		acct.Id = accountId
		acct.UserId = userId
		acct.GameId = am.g.ID
		acct.Name = accountName
		acct.SaveAccount()

	} else {
		// 更新game节点
		if acct.GameId != am.g.ID {
			acct.SaveGameNode(am.g.ID)
		}
	}

	// prometheus ops
	prom.OpsOnlineAccountGauge.Set(float64(am.cacheAccounts.ItemCount()))
	prom.OpsLogonAccountCounter.Inc()

	return acct, nil
}

func (am *AccountManager) startAccountTask(ctx context.Context, sock transport.Socket, acct *player.Account, start task.StartFn) {
	// account init task
	startFn := func() {
		// 增加连接
		am.Lock()
		am.mapSocks[sock] = acct.GetId()
		am.Unlock()
		acct.SetSock(sock)
		start()
	}
	stopFn := func() {
		// 删除连接
		am.Lock()
		delete(am.mapSocks, acct.GetSock())
		am.Unlock()
	}
	acct.InitTask(startFn, stopFn)

	am.wg.Wrap(func() {
		defer func() {
			if err := recover(); err != nil {
				stack := string(debug.Stack())
				log.Error().Caller().Msgf("catch exception:%v, panic recovered with stack:%s", err, stack)

				// 立即删除缓存
				am.cacheAccounts.Delete(acct.GetId())
			}
		}()

		log.Info().Caller().Int64("account_id", acct.GetId()).Str("remote_sock", sock.Remote()).Msg("account run new task")

		var errAcct error
		for {
			errAcct = acct.TaskRun(ctx)
			utils.ErrPrint(errAcct, "account run failed", acct.GetId())

			// pull up goroutine when task panic
			if errors.Is(errAcct, task.ErrTaskPanic) {
				continue
			} else {
				break
			}
		}

		// 被踢下线、连接超时、登陆失败，都立即删除缓存
		if errors.Is(errAcct, player.ErrAccountKicked) || errors.Is(errAcct, task.ErrTimeout) {
			am.cacheAccounts.Delete(acct.GetId())
			return
		}
	})
}

// LogonRequest encapsulates logon request parameters
type LogonRequest struct {
	UserID    int64
	Socket    transport.Socket
	Context   context.Context
	Timestamp time.Time
}

// LogonResult represents the result of a logon operation
type LogonResult struct {
	Account   *player.Account
	IsNewUser bool
	Error     error
}

// NewLogonRequest creates a new logon request
func NewLogonRequest(ctx context.Context, userID int64, sock transport.Socket) *LogonRequest {
	return &LogonRequest{
		UserID:    userID,
		Socket:    sock,
		Context:   ctx,
		Timestamp: time.Now(),
	}
}

// Logon handles user login with improved error handling and logging
func (am *AccountManager) Logon(ctx context.Context, userId int64, newSock transport.Socket) error {
	request := NewLogonRequest(ctx, userId, newSock)
	result := am.processLogon(request)

	if result.Error != nil {
		log.Error().
			Err(result.Error).
			Int64("user_id", userId).
			Str("socket_remote", newSock.Remote()).
			Msg("logon failed")
		return result.Error
	}

	log.Info().
		Int64("user_id", userId).
		Int64("account_id", result.Account.Id).
		Bool("is_new_user", result.IsNewUser).
		Str("socket_remote", newSock.Remote()).
		Msg("logon completed successfully")

	return nil
}

// processLogon handles the core logon logic
func (am *AccountManager) processLogon(request *LogonRequest) *LogonResult {
	// Step 1: Validate and get user information
	user, err := am.validateAndGetUser(request.UserID)
	if err != nil {
		return &LogonResult{Error: err}
	}

	// Step 2: Check if account exists in cache
	if cachedAccount := am.getCachedAccount(user.AccountID); cachedAccount != nil {
		return am.handleExistingAccount(request, cachedAccount)
	}

	// Step 3: Create new account
	return am.handleNewAccount(request, user)
}

// validateAndGetUser validates user ID and retrieves user information
func (am *AccountManager) validateAndGetUser(userID int64) (*User, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("invalid user ID: %d", userID)
	}

	user, err := am.getUser(userID)
	if err != nil {
		return nil, fmt.Errorf("failed to get user %d: %w", userID, err)
	}

	if user == nil {
		return nil, fmt.Errorf("user %d not found", userID)
	}

	return user, nil
}

// getCachedAccount retrieves account from cache
func (am *AccountManager) getCachedAccount(accountID int64) *player.Account {
	if cached, ok := am.cacheAccounts.Get(accountID); ok {
		return cached.(*player.Account)
	}
	return nil
}

// handleExistingAccount processes logon for existing cached account
func (am *AccountManager) handleExistingAccount(request *LogonRequest, account *player.Account) *LogonResult {
	log.Debug().
		Int64("account_id", account.Id).
		Str("socket_remote", request.Socket.Remote()).
		Msg("processing existing account logon")

	// Handle socket replacement if necessary
	if err := am.handleSocketReplacement(request, account); err != nil {
		return &LogonResult{Error: fmt.Errorf("failed to handle socket replacement: %w", err)}
	}

	// Start account task if not running
	if err := am.ensureAccountTaskRunning(request, account); err != nil {
		return &LogonResult{Error: fmt.Errorf("failed to start account task: %w", err)}
	}

	return &LogonResult{
		Account:   account,
		IsNewUser: false,
		Error:     nil,
	}
}

// handleSocketReplacement manages socket replacement for existing accounts
func (am *AccountManager) handleSocketReplacement(request *LogonRequest, account *player.Account) error {
	prevSock := account.GetSock()

	if prevSock != nil && prevSock != request.Socket {
		log.Info().
			Int64("account_id", account.Id).
			Str("prev_socket_remote", prevSock.Remote()).
			Str("new_socket_remote", request.Socket.Remote()).
			Msg("replacing existing socket connection")

		// Stop previous task gracefully
		account.TaskStop()

		// Close previous socket
		prevSock.Close()
	}

	return nil
}

// ensureAccountTaskRunning ensures the account task is running
func (am *AccountManager) ensureAccountTaskRunning(request *LogonRequest, account *player.Account) error {
	if !account.IsTaskRunning() {
		am.startAccountTask(request.Context, request.Socket, account, func() {
			account.LogonSucceed()
			log.Debug().
				Int64("account_id", account.Id).
				Msg("existing account logon succeeded")
		})
	}
	return nil
}

// handleNewAccount processes logon for new account
func (am *AccountManager) handleNewAccount(request *LogonRequest, user *User) *LogonResult {
	log.Debug().
		Int64("user_id", request.UserID).
		Int64("account_id", user.AccountID).
		Str("socket_remote", request.Socket.Remote()).
		Msg("creating new account")

	// Create new account
	account, err := am.createAndCacheAccount(request, user)
	if err != nil {
		return &LogonResult{Error: fmt.Errorf("failed to create account: %w", err)}
	}

	// Start account task with player loading
	if err := am.startNewAccountTask(request, account); err != nil {
		// Clean up on failure
		am.cacheAccounts.Delete(account.GetId())
		return &LogonResult{Error: fmt.Errorf("failed to start new account task: %w", err)}
	}

	return &LogonResult{
		Account:   account,
		IsNewUser: true,
		Error:     nil,
	}
}

// createAndCacheAccount creates a new account and adds it to cache
func (am *AccountManager) createAndCacheAccount(request *LogonRequest, user *User) (*player.Account, error) {
	account, err := am.newAccount(request.Context, request.UserID, user.AccountID, user.PlayerName, request.Socket)
	if err != nil {
		return nil, fmt.Errorf("failed to create new account for user %d: %w", request.UserID, err)
	}

	// Add to cache
	am.cacheAccounts.Set(account.GetId(), account, AccountCacheExpire)

	return account, nil
}

// startNewAccountTask starts task for new account with player loading
func (am *AccountManager) startNewAccountTask(request *LogonRequest, account *player.Account) error {
	am.startAccountTask(request.Context, request.Socket, account, func() {
		if err := am.handlePlayerLoading(request.Context, account); err != nil {
			log.Error().
				Err(err).
				Int64("account_id", account.Id).
				Msg("failed to load player for new account")

			// Clean up failed account
			am.cacheAccounts.Delete(account.GetId())
			return
		}

		account.LogonSucceed()
		log.Debug().
			Int64("account_id", account.Id).
			Msg("new account logon succeeded")
	})

	return nil
}

// handlePlayerLoading handles player loading for new accounts
func (am *AccountManager) handlePlayerLoading(ctx context.Context, account *player.Account) error {
	err := am.handleLoadPlayer(ctx, account)

	// Success or no player exists (both are acceptable)
	if err == nil || errors.Is(err, ErrAccountHasNoPlayer) {
		return nil
	}

	// Loading failed
	return fmt.Errorf("failed to load player for account %d: %w", account.Id, err)
}

func (am *AccountManager) GetAccountIdBySock(sock transport.Socket) (int64, bool) {
	am.RLock()
	defer am.RUnlock()

	id, ok := am.mapSocks[sock]
	return id, ok
}

func (am *AccountManager) GetAccountById(acctId int64) *player.Account {
	acct, ok := am.cacheAccounts.Get(acctId)
	if ok {
		return acct.(*player.Account)
	}

	return nil
}

func (am *AccountManager) GetPlayerInfoById(playerId int64) *player.PlayerInfo {
	am.Lock()
	defer am.Unlock()

	c, ok := am.cachePlayerInfos.Get(playerId)
	if ok {
		return c.(*player.PlayerInfo)
	}

	info := am.playerInfoPool.Get().(*player.PlayerInfo)
	info.Init()
	err := store.GetStore().FindOne(context.Background(), define.StoreType_Player, playerId, info)
	if err == nil {
		am.cachePlayerInfos.Set(playerId, info, PlayerInfoCacheExpire)
		return info
	}

	am.playerInfoPool.Put(info)
	return nil
}

// add handler to account's execute channel, will be dealed by account's run goroutine
func (am *AccountManager) AddAccountTask(ctx context.Context, acctId int64, fn task.TaskHandler, p ...any) error {
	acct := am.GetAccountById(acctId)

	if acct == nil {
		return fmt.Errorf("AddAccountTask err:%w, account_id:%d", ErrAccountNotFound, acctId)
	}

	if !acct.IsTaskRunning() {
		return ErrAccountTaskNotRunning
	}

	acct.AddTask(ctx, fn, p...)
	return nil
}

func (am *AccountManager) AddPlayerTask(ctx context.Context, playerId int64, fn task.TaskHandler, p ...any) error {
	info := am.GetPlayerInfoById(playerId)
	if info == nil {
		return fmt.Errorf("error:%w, player_id:%d", ErrPlayerInfoNotFound, playerId)
	}

	return am.AddAccountTask(ctx, info.AccountID, fn, p...)
}

// CreatePlayerRequest encapsulates player creation parameters
type CreatePlayerRequest struct {
	Account   *player.Account
	Name      string
	Context   context.Context
	Timestamp time.Time
}

// CreatePlayerResult represents the result of player creation
type CreatePlayerResult struct {
	Player *player.Player
	Error  error
}

// NewCreatePlayerRequest creates a new player creation request
func NewCreatePlayerRequest(ctx context.Context, account *player.Account, name string) *CreatePlayerRequest {
	return &CreatePlayerRequest{
		Account:   account,
		Name:      name,
		Context:   ctx,
		Timestamp: time.Now(),
	}
}

// CreatePlayer creates a new player for the given account with improved error handling
func (am *AccountManager) CreatePlayer(acct *player.Account, name string) (*player.Player, error) {
	request := NewCreatePlayerRequest(context.Background(), acct, name)
	result := am.processPlayerCreation(request)

	if result.Error != nil {
		log.Error().
			Err(result.Error).
			Int64("account_id", acct.Id).
			Str("player_name", name).
			Msg("player creation failed")
		return nil, result.Error
	}

	log.Info().
		Int64("account_id", acct.Id).
		Int64("player_id", result.Player.ID).
		Str("player_name", name).
		Msg("player created successfully")

	return result.Player, nil
}

// processPlayerCreation handles the core player creation logic
func (am *AccountManager) processPlayerCreation(request *CreatePlayerRequest) *CreatePlayerResult {
	// Step 1: Validate creation constraints
	if err := am.validatePlayerCreation(request.Account); err != nil {
		return &CreatePlayerResult{Error: err}
	}

	// Step 2: Generate unique player ID
	playerID, err := am.generatePlayerID()
	if err != nil {
		return &CreatePlayerResult{Error: fmt.Errorf("failed to generate player ID: %w", err)}
	}

	// Step 3: Create and initialize player object
	player, err := am.createPlayerObject(request, playerID)
	if err != nil {
		return &CreatePlayerResult{Error: fmt.Errorf("failed to create player object: %w", err)}
	}

	// Step 4: Save player data to database
	if err := am.savePlayerData(request.Context, player); err != nil {
		// Clean up on failure
		am.playerPool.Put(player)
		return &CreatePlayerResult{Error: fmt.Errorf("failed to save player data: %w", err)}
	}

	// Step 5: Update account with player information
	if err := am.linkPlayerToAccount(request.Context, request.Account, player); err != nil {
		log.Warn().
			Int64("account_id", request.Account.Id).
			Int64("player_id", player.ID).
			Err(err).
			Msg("failed to update account, but player was created successfully")
	}

	// Step 6: Initialize player for first login
	am.initializeNewPlayer(player)

	return &CreatePlayerResult{Player: player, Error: nil}
}

// validatePlayerCreation checks if player creation is allowed
func (am *AccountManager) validatePlayerCreation(account *player.Account) error {
	if account == nil {
		return fmt.Errorf("account cannot be nil")
	}

	// Only allow one player per account
	if existingPlayer := account.GetPlayer(); existingPlayer != nil {
		return player.ErrCreateMoreThanOnePlayer
	}

	return nil
}

// generatePlayerID creates a unique player ID
func (am *AccountManager) generatePlayerID() (int64, error) {
	id, err := utils.NextID(define.SnowFlake_Player)
	if err != nil {
		return 0, fmt.Errorf("snowflake ID generation failed: %w", err)
	}
	return id, nil
}

// createPlayerObject initializes a new player object
func (am *AccountManager) createPlayerObject(request *CreatePlayerRequest, playerID int64) (*player.Player, error) {
	// Get player object from pool
	p := am.playerPool.Get().(*player.Player)

	// Initialize player
	p.Init(playerID)
	p.AccountID = request.Account.Id
	p.SetAccount(request.Account)
	p.SetName(request.Name)

	log.Debug().
		Int64("player_id", playerID).
		Int64("account_id", request.Account.Id).
		Str("name", request.Name).
		Msg("player object created")

	return p, nil
}

// savePlayerData persists player data to database
func (am *AccountManager) savePlayerData(ctx context.Context, player *player.Player) error {
	// Define all save operations
	saveOperations := []struct {
		name string
		fn   func() error
	}{
		{
			name: "player_basic_info",
			fn: func() error {
				return store.GetStore().UpdateOne(ctx, define.StoreType_Player, player.ID, player)
			},
		},
		{
			name: "player_tokens",
			fn: func() error {
				return store.GetStore().UpdateOne(ctx, define.StoreType_Token, player.ID, player.TokenManager())
			},
		},
		{
			name: "player_fragments",
			fn: func() error {
				return store.GetStore().UpdateOne(ctx, define.StoreType_Fragment, player.ID, player.FragmentManager())
			},
		},
		// TODO: Uncomment when ready
		// {
		//     name: "player_heroes",
		//     fn: func() error {
		//         return store.GetStore().UpdateOne(ctx, define.StoreType_Hero, player.ID, player.HeroManager())
		//     },
		// },
		// {
		//     name: "player_items",
		//     fn: func() error {
		//         return store.GetStore().UpdateOne(ctx, define.StoreType_Item, player.ID, player.ItemManager())
		//     },
		// },
	}

	// Execute all save operations
	for _, op := range saveOperations {
		if err := op.fn(); err != nil {
			return fmt.Errorf("failed to save %s for player %d: %w", op.name, player.ID, err)
		}

		log.Debug().
			Int64("player_id", player.ID).
			Str("operation", op.name).
			Msg("player data saved successfully")
	}

	return nil
}

// linkPlayerToAccount updates account with player information
func (am *AccountManager) linkPlayerToAccount(ctx context.Context, account *player.Account, player *player.Player) error {
	// Update account with player information
	account.SetPlayer(player)
	account.Name = player.GetName()
	account.Level = player.GetLevel()
	account.AddPlayerID(player.GetId())

	// Save updated account
	if err := store.GetStore().UpdateOne(ctx, define.StoreType_Account, account.Id, account, true); err != nil {
		return fmt.Errorf("failed to update account %d: %w", account.Id, err)
	}

	log.Debug().
		Int64("account_id", account.Id).
		Int64("player_id", player.ID).
		Msg("player linked to account successfully")

	return nil
}

// initializeNewPlayer performs first-time player initialization
func (am *AccountManager) initializeNewPlayer(player *player.Player) {
	// First login processing
	player.OnFirstLogon()

	// Send initial player information to client
	player.SendInitInfo()

	log.Debug().
		Int64("player_id", player.ID).
		Msg("new player initialized")
}

func (am *AccountManager) Broadcast(msg proto.Message) {
	am.cacheAccounts.Range(func(v any) bool {
		acct := v.(*cache.Item).Object.(*player.Account)
		acct.AddTask(context.Background(), func(c context.Context, p ...any) error {
			a := p[0].(*player.Account)
			message := p[1].(proto.Message)
			a.SendProtoMessage(message)
			return nil
		}, msg)
		return true
	})
}

func (am *AccountManager) Run(ctx context.Context) error {
	<-ctx.Done()
	log.Info().Msg("world session context done...")
	return nil
}
