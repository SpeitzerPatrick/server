# 评论系统缓存设计详解

评论系统的缓存设计是整个服务性能和可扩展性的关键。本文将详细介绍评论系统中的缓存机制、实现原理和优化策略。

## 1. 缓存架构概述

评论系统采用多层缓存架构，包括内存缓存和对象池，以提高数据访问性能并减少资源消耗：

1. **内存缓存**：使用自定义的`cache.Cache`实现，缓存热门评论话题数据
2. **对象池**：使用`sync.Pool`实现，复用`CommentTopicData`对象
3. **有序集合**：使用`zset.SortedSet`实现，高效存储和排序评论数据

这种多层缓存架构能够显著提高评论系统的性能，减少数据库访问，并降低内存分配和GC压力。

## 2. 内存缓存实现

### 2.1 缓存配置

评论系统使用以下配置参数定义缓存行为：

```go
var (
    commentCleanupInterval       = 1 * time.Minute // cache cleanup interval
    commentCacheExpire           = 1 * time.Hour   // cache缓存1小时
    commentDefaultLoad     int64 = 10              // 默认加载前10条评论
)
```

这些参数定义了：
- 缓存清理间隔：1分钟
- 缓存过期时间：1小时
- 默认加载评论数：10条

### 2.2 缓存初始化

在`CommentManager`初始化时创建缓存：

```go
func NewCommentManager(ctx *cli.Context, r *Comment) *CommentManager {
    manager := &CommentManager{
        r:                 r,
        cacheCommentDatas: cache.New(commentCacheExpire, commentCleanupInterval),
    }

    // 评论缓存删除时处理
    manager.cacheCommentDatas.OnEvicted(func(k, v any) {
        v.(*CommentTopicData).Stop()
        manager.commentPool.Put(v)
    })
    
    // ...其他初始化代码...
    
    return manager
}
```

缓存初始化包括：
- 创建缓存实例，设置过期时间和清理间隔
- 设置缓存项被移除时的回调函数，确保资源正确释放

### 2.3 缓存实现原理

评论系统使用的`cache.Cache`是一个基于Go标准库的自定义缓存实现，其核心数据结构如下：

```go
type Cache struct {
    *cache
}

type cache struct {
    defaultExpiration time.Duration
    items             map[interface{}]*Item
    mu                sync.RWMutex
    onEvicted         func(interface{}, interface{})
    janitor           *janitor
}

type Item struct {
    Object     interface{}
    Expiration int64
}
```

缓存的主要特点：
- 使用`map`存储缓存项，支持任意类型的键和值
- 每个缓存项包含对象和过期时间
- 使用读写锁保护并发访问
- 支持设置缓存项被移除时的回调函数
- 使用后台清理器(`janitor`)定期清理过期项

### 2.4 缓存操作

#### 2.4.1 设置缓存

```go
func (c *cache) Set(k interface{}, x interface{}, d time.Duration) {
    c.mu.Lock()
    c.set(k, x, d)
    c.mu.Unlock()
}

func (c *cache) set(k interface{}, x interface{}, d time.Duration) {
    var e int64
    if d == DefaultExpiration {
        d = c.defaultExpiration
    }
    if d > 0 {
        e = time.Now().Add(d).UnixNano()
    }

    if _, ok := c.get(k); ok {
        c.items[k].Object = x
        c.items[k].Expiration = e
    } else {
        c.items[k] = &Item{
            Object:     x,
            Expiration: e,
        }
    }
}
```

设置缓存的过程：
1. 获取写锁，确保并发安全
2. 计算过期时间
3. 如果键已存在，更新值和过期时间
4. 如果键不存在，创建新的缓存项

#### 2.4.2 获取缓存

```go
func (c *cache) Get(k interface{}) (interface{}, bool) {
    c.mu.RLock()
    // "Inlining" of get and Expired
    item, found := c.items[k]
    if !found {
        c.mu.RUnlock()
        return nil, false
    }
    if item.Expiration > 0 {
        if time.Now().UnixNano() > item.Expiration {
            c.mu.RUnlock()
            return nil, false
        }
        item.Expiration = time.Now().Add(c.defaultExpiration).UnixNano()
    }
    c.mu.RUnlock()
    return item.Object, true
}
```

获取缓存的过程：
1. 获取读锁，确保并发安全
2. 查找缓存项
3. 如果未找到，返回`nil`和`false`
4. 如果找到但已过期，返回`nil`和`false`
5. 如果找到且未过期，**更新过期时间**并返回对象和`true`

注意：获取缓存时会自动更新过期时间，这是一种"访问时刷新"的策略，确保频繁访问的缓存项不会过期。

#### 2.4.3 删除缓存

```go
func (c *cache) Delete(k interface{}) {
    c.mu.Lock()
    v, evicted := c.delete(k)
    c.mu.Unlock()
    if evicted {
        c.onEvicted(k, v)
    }
}

func (c *cache) delete(k interface{}) (interface{}, bool) {
    if c.onEvicted != nil {
        if v, found := c.items[k]; found {
            delete(c.items, k)
            return v.Object, true
        }
    }
    delete(c.items, k)
    return nil, false
}
```

删除缓存的过程：
1. 获取写锁，确保并发安全
2. 删除缓存项
3. 如果缓存项存在且设置了回调函数，调用回调函数

#### 2.4.4 清理过期缓存

```go
func (c *cache) DeleteExpired() {
    var evictedItems []keyAndValue
    now := time.Now().UnixNano()
    c.mu.Lock()
    for k, v := range c.items {
        // "Inlining" of expired
        if v.Expiration > 0 && now > v.Expiration {
            ov, evicted := c.delete(k)
            if evicted {
                evictedItems = append(evictedItems, keyAndValue{k, ov})
            }
        }
    }
    c.mu.Unlock()
    for _, v := range evictedItems {
        c.onEvicted(v.key, v.value)
    }
}
```

清理过期缓存的过程：
1. 获取写锁，确保并发安全
2. 遍历所有缓存项，检查是否过期
3. 删除过期的缓存项，并记录被删除的项
4. 释放写锁
5. 对每个被删除的项调用回调函数

### 2.5 后台清理器

缓存系统使用后台清理器定期清理过期项：

```go
type janitor struct {
    Interval time.Duration
    stop     chan bool
}

func (j *janitor) Run(c *cache) {
    ticker := time.NewTicker(j.Interval)
    for {
        select {
        case <-ticker.C:
            c.DeleteExpired()
        case <-j.stop:
            ticker.Stop()
            return
        }
    }
}

func runJanitor(c *cache, ci time.Duration) {
    j := &janitor{
        Interval: ci,
        stop:     make(chan bool),
    }
    c.janitor = j
    go j.Run(c)
}
```

后台清理器的工作原理：
1. 创建一个定时器，间隔为配置的清理间隔
2. 在每个时间间隔调用`DeleteExpired`方法清理过期项
3. 支持通过`stop`通道停止清理

## 3. 评论话题数据缓存

评论系统使用`CommentManager.cacheCommentDatas`缓存评论话题数据：

```go
func (m *CommentManager) getCommentData(topic define.CommentTopic) (*CommentTopicData, error) {
    if !topic.Valid() {
        return nil, ErrInvalidComment
    }

    m.mu.Lock()
    defer m.mu.Unlock()

    topicId := utils.PackId(topic.Type, topic.TypeId)
    cache, ok := m.cacheCommentDatas.Get(topicId)

    if ok {
        rd := cache.(*CommentTopicData)
        if rd.IsTaskRunning() {
            return rd, nil
        }

    } else {
        // 缓存没有，从db加载
        cache = m.commentPool.Get()
        cd := cache.(*CommentTopicData)
        cd.Init(m.r.ID, m.r.rpcHandler)
        err := cd.Load(topic)
        if !utils.ErrCheck(err, "CommentTopicData Load failed when CommentManager.getCommentData", topic) {
            m.commentPool.Put(cache)
            return nil, err
        }

        // 踢掉上一个节点的缓存
        if cd.LastSaveNodeId != -1 && cd.LastSaveNodeId != int32(m.r.ID) {
            err := m.KickCommentTopicData(topic, cd.LastSaveNodeId)
            if !utils.ErrCheck(err, "kick CommentTopicData failed", topic, cd.LastSaveNodeId, m.r.ID) {
                return nil, err
            }
        }

        m.cacheCommentDatas.Set(topicId, cache, commentCacheExpire)
    }

    // ...初始化任务系统和启动任务处理协程...

    return cd, nil
}
```

评论话题数据缓存的特点：
1. 使用`topic.Type`和`topic.TypeId`组合作为缓存键
2. 缓存命中时，检查任务系统是否正在运行
3. 缓存未命中时，从对象池获取`CommentTopicData`对象，并从数据库加载数据
4. 如果数据之前由其他节点管理，踢掉该节点的缓存
5. 将加载的数据放入缓存，设置过期时间为1小时

## 4. 对象池设计

评论系统使用`sync.Pool`实现对象池，复用`CommentTopicData`对象：

```go
func NewCommentManager(ctx *cli.Context, r *Comment) *CommentManager {
    manager := &CommentManager{
        r:                 r,
        cacheCommentDatas: cache.New(commentCacheExpire, commentCleanupInterval),
    }

    // 评论池
    manager.commentPool.New = NewCommentData

    // ...其他初始化代码...
    
    return manager
}

func NewCommentData() any {
    return &CommentTopicData{}
}
```

对象池的工作原理：
1. 设置`commentPool.New`函数，用于创建新的`CommentTopicData`对象
2. 当需要`CommentTopicData`对象时，从池中获取：`cache = m.commentPool.Get()`
3. 当对象不再使用时，放回池中：`m.commentPool.Put(cache)`

对象池的优势：
1. 减少内存分配和GC压力
2. 提高对象复用率
3. 降低内存碎片

## 5. 缓存一致性

在分布式环境中，评论系统需要确保缓存的一致性。评论系统使用以下机制维护缓存一致性：

### 5.1 最后保存节点记录

每个`CommentTopicData`记录最后保存它的节点ID：

```go
type CommentTopicData struct {
    define.CommentTopic `json:"_id" bson:"_id"` // 评论话题
    LastSaveNodeId      int32                   `json:"last_save_node_id" bson:"last_save_node_id"`
    // ...其他字段...
}
```

当节点修改评论数据时，会更新`LastSaveNodeId`：

```go
func (c *CommentTopicData) saveLastNode() {
    c.LastSaveNodeId = int32(c.NodeId)
    err := store.GetStore().UpdateOne(context.Background(), define.StoreType_Comment, c.CommentTopic, c)
    _ = utils.ErrCheck(err, "UpdateOne failed when CommentTopicData.saveLastNode", c.CommentTopic)
}
```

### 5.2 踢出其他节点缓存

当节点加载评论话题数据时，如果发现数据之前由其他节点管理，会踢掉该节点的缓存：

```go
// 踢掉上一个节点的缓存
if cd.LastSaveNodeId != -1 && cd.LastSaveNodeId != int32(m.r.ID) {
    err := m.KickCommentTopicData(topic, cd.LastSaveNodeId)
    if !utils.ErrCheck(err, "kick CommentTopicData failed", topic, cd.LastSaveNodeId, m.r.ID) {
        return nil, err
    }
}
```

踢出缓存的实现：

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

踢出缓存的过程：
1. 如果是本节点，直接停止任务并清除缓存
2. 如果是其他节点，通过RPC调用踢出其他节点的缓存

### 5.3 缓存项被移除时的处理

当缓存项被移除时（过期或手动删除），评论系统会执行以下操作：

```go
manager.cacheCommentDatas.OnEvicted(func(k, v any) {
    v.(*CommentTopicData).Stop()
    manager.commentPool.Put(v)
})
```

这确保了：
1. 停止与缓存项关联的任务
2. 将`CommentTopicData`对象放回对象池，以便复用

## 6. 缓存优化策略

### 6.1 访问时刷新

评论系统的缓存实现了"访问时刷新"策略：

```go
func (c *cache) Get(k interface{}) (interface{}, bool) {
    // ...其他代码...
    if item.Expiration > 0 {
        if time.Now().UnixNano() > item.Expiration {
            c.mu.RUnlock()
            return nil, false
        }
        item.Expiration = time.Now().Add(c.defaultExpiration).UnixNano()
    }
    // ...其他代码...
}
```

每次访问缓存项时，如果缓存项未过期，会更新其过期时间。这确保了频繁访问的缓存项不会过期，提高了热点数据的缓存命中率。

### 6.2 延迟加载

评论系统使用延迟加载策略，只有在需要访问评论话题数据时才从数据库加载：

```go
func (m *CommentManager) getCommentData(topic define.CommentTopic) (*CommentTopicData, error) {
    // ...其他代码...
    if !ok {
        // 缓存没有，从db加载
        cache = m.commentPool.Get()
        cd := cache.(*CommentTopicData)
        cd.Init(m.r.ID, m.r.rpcHandler)
        err := cd.Load(topic)
        // ...其他代码...
    }
    // ...其他代码...
}
```

延迟加载的优势：
1. 只加载实际需要的数据，减少内存使用
2. 避免启动时加载大量数据，提高启动速度
3. 更好地适应动态变化的访问模式

### 6.3 资源释放

评论系统在缓存项被移除时释放资源：

```go
manager.cacheCommentDatas.OnEvicted(func(k, v any) {
    v.(*CommentTopicData).Stop()
    manager.commentPool.Put(v)
})
```

这确保了：
1. 停止与缓存项关联的任务，避免资源泄漏
2. 将对象放回对象池，以便复用

### 6.4 定期清理

评论系统使用后台清理器定期清理过期的缓存项：

```go
commentCleanupInterval = 1 * time.Minute // cache cleanup interval
```

定期清理的优势：
1. 自动释放不再需要的资源
2. 避免缓存无限增长
3. 减少内存使用

## 7. 缓存监控和管理

评论系统提供了以下方法监控和管理缓存：

### 7.1 踢出所有缓存

```go
func (m *CommentManager) KickAllCommentTopicData() {
    m.cacheCommentDatas.DeleteAll()
}
```

这个方法会删除所有缓存的评论话题数据，通常在服务重启或需要刷新所有缓存时使用。

### 7.2 踢出特定话题缓存

```go
func (m *CommentManager) KickCommentTopicData(topic define.CommentTopic, commentNodeId int32) error {
    // ...实现代码...
}
```

这个方法会删除特定评论话题的缓存，通常在数据更新或需要刷新特定缓存时使用。

### 7.3 健康检查

评论系统在服务健康检查时会踢出所有缓存：

```go
server.RegisterCheck(func(context.Context) error {
    _, err := s.srv.Server().Options().Registry.GetService("comment")
    rAddrs := s.srv.Server().Options().Registry.Options().Addrs
    if !utils.ErrCheck(err, "GetService failed when RegisterCheck", rAddrs) {
        s.m.manager.KickAllCommentTopicData()
    }
    return err
}),
```

这确保了在服务状态异常时清除所有缓存，避免使用过时的数据。

## 8. 缓存与数据库交互

### 8.1 数据加载

评论系统从数据库加载评论话题数据：

```go
func (c *CommentTopicData) Load(topic define.CommentTopic) error {
    // 加载话题信息
    err := store.GetStore().FindOne(context.Background(), define.StoreType_Comment, topic, c)

    // 创建新评论数据
    if errors.Is(err, store.ErrNoResult) {
        c.CommentTopic = topic
        c.LastSaveNodeId = int32(c.NodeId)
        errSave := store.GetStore().UpdateOne(context.Background(), define.StoreType_Comment, topic, c, true)
        utils.ErrPrint(errSave, "UpdateOne failed when CommentTopicData.Load", topic)
        return errSave
    }

    if !utils.ErrCheck(err, "FindOne failed when CommentTopicData.Load", topic) {
        return err
    }

    // 加载评论数据
    res, err := store.GetStore().FindAll(context.Background(), define.StoreType_Rank, "topic", topic)
    if !utils.ErrCheck(err, "FindAll failed when CommentTopicData.Load", topic) {
        return err
    }

    for _, v := range res {
        vv := v.([]byte)
        metadata := &define.CommentMetadata{}
        err := json.Unmarshal(vv, metadata)
        if !utils.ErrCheck(err, "json.Unmarshal failed when CommentTopicData.Load", vv) {
            continue
        }

        c.zsets.Set(
            float64(metadata.PublisherMetadata.Thumbs),
            metadata.CommentId,
            int64(metadata.PublisherMetadata.Date),
            metadata,
        )
    }

    return nil
}
```

数据加载过程：
1. 从数据库加载话题基本信息
2. 如果话题不存在，创建新的话题记录
3. 加载该话题下的所有评论数据
4. 将评论数据添加到有序集合中，按点赞数排序

### 8.2 数据保存

评论系统在修改评论数据后将其保存到数据库：

```go
func (c *CommentTopicData) ModThumbs(ctx context.Context, commentId int64, modThumbs int32) error {
    // ...其他代码...

    // save comment metadata
    err := store.GetStore().UpdateOne(ctx, define.StoreType_Comment, cm.CommentId, cm)
    _ = utils.ErrCheck(err, "UpdateOne failed when CommentTopicData.ModThumbs", cm)

    c.saveLastNode()
    return err
}
```

数据保存过程：
1. 更新评论数据
2. 将更新后的数据保存到数据库
3. 更新最后保存节点ID

## 9. 缓存设计的优势与挑战

### 9.1 优势

1. **高性能**：内存缓存提供快速的数据访问，减少数据库查询
2. **资源效率**：对象池减少内存分配和GC压力
3. **可扩展性**：支持分布式部署，通过RPC协调多个节点的缓存
4. **一致性**：通过最后保存节点记录和踢出缓存机制维护缓存一致性
5. **自动管理**：定期清理过期项，自动释放资源

### 9.2 挑战

1. **内存使用**：缓存占用内存，需要合理设置过期时间和清理策略
2. **一致性维护**：在分布式环境中维护缓存一致性需要额外的协调机制
3. **复杂性**：多层缓存架构增加了系统复杂性
4. **监控和调优**：需要监控缓存命中率和内存使用，并进行调优

## 10. 总结

评论系统的缓存设计是一个多层次、高性能的架构，包括内存缓存、对象池和有序集合。通过精心设计的缓存策略和一致性维护机制，评论系统能够提供高性能、可扩展的评论数据管理服务。

主要特点包括：
1. 使用`cache.Cache`实现内存缓存，支持过期时间和自动清理
2. 使用`sync.Pool`实现对象池，减少内存分配和GC压力
3. 使用"访问时刷新"策略，提高热点数据的缓存命中率
4. 使用延迟加载策略，只加载实际需要的数据
5. 通过最后保存节点记录和踢出缓存机制维护缓存一致性
6. 在缓存项被移除时释放资源，避免资源泄漏
7. 提供缓存监控和管理功能，支持踢出所有缓存或特定话题缓存

这些设计和优化使评论系统能够支持大规模游戏应用中的评论功能需求，提供高性能、可靠的评论数据管理服务。