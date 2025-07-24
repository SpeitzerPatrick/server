# 评论服务总结

评论服务是一个独立的微服务，负责管理游戏中各种话题的评论数据。它提供了评论的创建、查询、点赞等功能，支持分布式部署和高并发访问。

## 核心数据结构

1. **Comment 结构体**
   - 服务的主体结构，包含基本配置和状态管理
   - 包含服务ID、雪花算法起始时间等基础信息
   - 管理其他组件如CommentManager、RpcHandler等

```go
type Comment struct {
    app                *cli.App `bson:"-" json:"-"`
    ID                 int16    `bson:"_id" json:"_id"`
    SnowflakeStartTime int64    `bson:"snowflake_starttime" json:"snowflake_starttime"`
    sync.RWMutex       `bson:"-" json:"-"`
    wg                 utils.WaitGroupWrapper `bson:"-" json:"-"`

    gin        *GinServer             `bson:"-" json:"-"`
    manager    *CommentManager        `bson:"-" json:"-"`
    mi         *MicroService          `bson:"-" json:"-"`
    rpcHandler *RpcHandler            `bson:"-" json:"-"`
    pubSub     *PubSub                `bson:"-" json:"-"`
    cons       *consistent.Consistent `bson:"-" json:"-"`
}
```

2. **CommentManager 结构体**
   - 管理评论数据的核心组件
   - 使用缓存提高查询性能
   - 实现评论数据的池化管理

```go
type CommentManager struct {
    r                 *Comment
    cacheCommentDatas *cache.Cache
    commentPool       sync.Pool
    wg                utils.WaitGroupWrapper
    mu                sync.Mutex
}
```

3. **CommentTopicData 结构体**
   - 表示单个评论话题的数据
   - 包含评论话题类型、ID和所属节点信息
   - 使用有序集合(zset)存储评论，支持按点赞数排序

```go
type CommentTopicData struct {
    define.CommentTopic `json:"_id" bson:"_id"` // 评论话题
    LastSaveNodeId      int32                   `json:"last_save_node_id" bson:"last_save_node_id"`
    NodeId              int16                   `json:"-" bson:"-"` // 当前节点id
    zsets               *zset.SortedSet         `json:"-" bson:"-"` // 评论按赞排行
    tasker              *task.Tasker            `json:"-" bson:"-"`
    rpcHandler          *RpcHandler             `json:"-" bson:"-"`
}
```

## 主要行为

1. **服务初始化与配置**
   - 通过`New()`创建服务实例
   - 使用CLI应用框架处理命令行参数和配置文件
   - 初始化雪花算法生成唯一ID
   - 设置数据库连接和缓存系统

```go
func (m *Comment) initSnowflake() {
    store.GetStore().AddStoreInfo(define.StoreType_Machine, "machine", "_id")
    if err := store.GetStore().MigrateDbTable("machine"); err != nil {
        log.Fatal().Err(err).Msg("migrate collection machine failed")
    }

    err := store.GetStore().FindOne(context.Background(), define.StoreType_Machine, m.ID, m)
    if err != nil && !errors.Is(err, store.ErrNoResult) {
        log.Fatal().Err(err).Msg("FindOne failed when Comment.initSnowflake")
    }

    utils.InitMachineID(m.ID, m.SnowflakeStartTime, func() {
        m.SnowflakeStartTime = time.Now().Unix()
        err := store.GetStore().UpdateOne(context.Background(), define.StoreType_Machine, m.ID, m)
        _ = utils.ErrCheck(err, "UpdateOne failed when NextID", m.ID)
    })
}
```

2. **评论数据管理**
   - 加载和缓存评论话题数据
   - 实现评论的添加、查询和修改
   - 支持按范围查询评论
   - 管理评论点赞数

```go
func (m *CommentManager) QueryCommentTopic(ctx context.Context, topic define.CommentTopic) (metadatas []*define.CommentMetadata, err error) {
    err = m.AddTask(
        ctx,
        topic,
        func(c context.Context, p ...any) error {
            var e error
            ctd := p[0].(*CommentTopicData)
            metadatas, e = ctd.GetCommentByRange(c, 0, commentDefaultLoad)
            return e
        },
    )

    _ = utils.ErrCheck(err, "AddTask failed when CommentManager.QueryCommentTopic", topic)
    return
}
```

3. **任务系统**
   - 使用任务队列处理评论操作
   - 支持任务超时和错误恢复
   - 实现任务的并发控制

```go
func (c *CommentTopicData) AddTask(ctx context.Context, fn task.TaskHandler, p ...any) error {
    return c.tasker.AddWait(ctx, fn, p...)
}
```

4. **分布式协调**
   - 使用一致性哈希算法分配评论话题
   - 实现节点间评论数据同步
   - 支持踢出其他节点的缓存

```go
func (m *CommentManager) KickCommentTopicData(topic define.CommentTopic, commentNodeId int32) error {
    if !topic.Valid() {
        return nil
    }

    // 踢掉本服CommentTopicData
    if commentNodeId == int32(m.r.ID) {
        topicId := utils.PackId(topic.Type, topic.TypeId)
        cd, ok := m.cacheCommentDatas.Get(topicId)
        if !ok {
            return nil
        }

        cd.(*CommentTopicData).Stop()
        store.GetStore().Flush()
        return nil

    } else {
        // comment节点不存在的话不用发送rpc
        nodeId := fmt.Sprintf("comment-%d", commentNodeId)
        srvs, err := m.r.mi.srv.Options().Registry.GetService("comment")
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

        // 发送rpc踢掉其他服CommentTopicData
        rs, err := m.r.rpcHandler.CallKickCommentTopicData(topic, commentNodeId)
        if !utils.ErrCheck(err, "kick comment topic data failed", topic, commentNodeId, rs) {
            return err
        }

        // rpc调用成功
        if rs.GetTopic().GetTopicType() == topic.Type && rs.GetTopic().GetTopicTypeId() == topic.TypeId {
            return nil
        }

        return errors.New("kick comment topic data invalid error")
    }
}
```

5. **RPC接口**
   - 提供评论查询、修改等RPC接口
   - 实现跨服务通信
   - 处理评论点赞请求

```go
func (h *RpcHandler) QueryCommentTopic(
    ctx context.Context,
    req *pbComment.QueryCommentTopicRq,
    rsp *pbComment.QueryCommentTopicRs,
) error {
    var topic define.CommentTopic
    topic.FromPB(req.GetTopic())
    metadatas, err := h.m.manager.QueryCommentTopic(ctx, topic)
    if utils.ErrCheck(err, "QueryCommentTopic failed when RpcHandler.QueryCommentTopic") {
        return err
    }

    rsp.Metadatas = make([]*pbGlobal.CommentMetadata, 0, len(metadatas))
    for _, v := range metadatas {
        rsp.Metadatas = append(rsp.Metadatas, v.ToPB())
    }
    return err
}
```

## 技术特点

1. **缓存机制**
   - 使用内存缓存提高查询性能
   - 实现缓存过期和清理策略
   - 使用对象池减少内存分配

```go
func NewCommentManager(ctx *cli.Context, r *Comment) *CommentManager {
    manager := &CommentManager{
        r:                 r,
        cacheCommentDatas: cache.New(commentCacheExpire, commentCleanupInterval),
    }

    // 评论池
    manager.commentPool.New = NewCommentData

    // 评论缓存删除时处理
    manager.cacheCommentDatas.OnEvicted(func(k, v any) {
        v.(*CommentTopicData).Stop()
        manager.commentPool.Put(v)
    })

    // 初始化db
    store.GetStore().AddStoreInfo(define.StoreType_Comment, "comment", "_id")
    if err := store.GetStore().MigrateDbTable("comment"); err != nil {
        log.Fatal().Err(err).Msg("migrate collection comment failed")
    }

    log.Info().Msg("CommentManager init ok ...")
    return manager
}
```

2. **微服务架构**
   - 基于go-micro实现服务注册、发现和通信
   - 提供gRPC接口供其他服务调用
   - 支持服务健康检查和监控

```go
func NewMicroService(ctx *cli.Context, m *Comment) *MicroService {
    // cert
    certPath := ctx.String("cert_path_release")
    keyPath := ctx.String("key_path_release")

    if ctx.Bool("debug") {
        certPath = ctx.String("cert_path_debug")
        keyPath = ctx.String("key_path_debug")
    }

    tlsConf := &tls.Config{InsecureSkipVerify: true}
    cert, err := tls.LoadX509KeyPair(certPath, keyPath)
    if err != nil {
        log.Fatal().
            Err(err).
            Msg("load certificates failed")
    }
    tlsConf.Certificates = []tls.Certificate{cert}

    err = micro_logger.Init(micro_logger.WithOutput(logger.Logger))
    if err != nil {
        log.Fatal().Err(err).Msg("micro_logger init failed")
    }

    s := &MicroService{
        m:         m,
        entryList: make([]map[string]int, 0),
    }

    bucket := juju_ratelimit.NewBucket(ctx.Duration("rate_limit_interval"), int64(ctx.Int("rate_limit_capacity")))
    s.srv = micro.NewService(
        micro.Server(
            grpc_server.NewServer(
                server.WrapHandler(ratelimit.NewHandlerWrapper(bucket, false)),
                server.RegisterCheck(func(context.Context) error {
                    _, err := s.srv.Server().Options().Registry.GetService("comment")
                    rAddrs := s.srv.Server().Options().Registry.Options().Addrs
                    if !utils.ErrCheck(err, "GetService failed when RegisterCheck", rAddrs) {
                        s.m.manager.KickAllCommentTopicData()
                    }
                    return err
                }),
            ),
        ),

        micro.Client(
            grpc_client.NewClient(),
        ),

        micro.Name("comment"),
        micro.WrapHandler(prometheus.NewHandlerWrapper()),

        micro.Transport(tcp.NewTransport(
            transport.TLSConfig(tlsConf),
        )),
    )
```

3. **HTTP/HTTPS API**
   - 基于Gin框架提供RESTful API
   - 支持监控、调试和管理功能
   - 实现请求限流和安全控制

```go
func (s *GinServer) setupHttpRouter() {
    s.router.Use(limit.MaxAllowed(ginConcurrentRequestLimit))
    s.router.Use(gin.LoggerWithWriter(logger.Logger))
    
    // 添加Prometheus监控端点
    s.router.GET("/metrics", ginHandlerWrapper(promhttp.Handler().ServeHTTP))
}
```

## 性能优化

1. **内存管理**
   - 使用对象池减少GC压力
   - 实现缓存过期策略释放内存
   - 使用有序集合高效存储和排序评论

2. **并发控制**
   - 使用互斥锁保护共享数据
   - 实现任务队列避免并发冲突
   - 使用等待组管理goroutine生命周期

3. **错误处理**
   - 捕获和恢复panic
   - 详细的错误日志和监控
   - 实现错误重试机制

评论服务设计合理，具有良好的可扩展性和性能特性，能够支持大规模游戏应用中的评论功能需求。它通过缓存机制、任务系统和分布式协调等技术，实现了高效的评论数据管理和查询功能。