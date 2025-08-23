# 评论功能缓存过期处理详解

## 概述

评论功能实现了完整的缓存过期处理机制，包括自动过期清理、访问时刷新、资源清理和分布式缓存管理等功能。本文档详细介绍评论系统的缓存过期处理实现。

## 1. 缓存过期配置

### 1.1 基础配置参数

```go
var (
    commentCleanupInterval = 1 * time.Minute // 缓存清理间隔：1分钟
    commentCacheExpire     = 1 * time.Hour   // 缓存过期时间：1小时
    commentDefaultLoad     = 10              // 默认加载前10条评论
)
```

**配置说明**：
- **过期时间**：1小时，平衡性能和数据新鲜度
- **清理间隔**：1分钟，及时释放过期资源
- **默认加载**：10条评论，避免一次性加载过多数据

### 1.2 缓存初始化

```go
func NewCommentManager(ctx *cli.Context, r *Comment) *CommentManager {
    manager := &CommentManager{
        r:                 r,
        // 创建缓存：过期时间1小时，清理间隔1分钟
        cacheCommentDatas: cache.New(commentCacheExpire, commentCleanupInterval),
    }

    // 缓存项被移除时的处理（过期或手动删除）
    manager.cacheCommentDatas.OnEvicted(func(k, v any) {
        v.(*CommentTopicData).Stop()    // 停止相关任务
        manager.commentPool.Put(v)      // 放回对象池复用
    })

    return manager
}
```

## 2. 多层过期处理机制

### 2.1 自动过期清理（Janitor机制）

```go
type janitor struct {
    Interval time.Duration  // 清理间隔
    stop     chan bool
}

func (j *janitor) Run(c *cache) {
    ticker := time.NewTicker(j.Interval)
    for {
        select {
        case <-ticker.C:
            c.DeleteExpired()  // 定期清理过期项
        case <-j.stop:
            ticker.Stop()
            return
        }
    }
}
```

**特点**：
- **后台运行**：独立的goroutine定期清理
- **可控制**：通过stop channel优雅停止
- **高效**：批量处理过期项，减少锁竞争

### 2.2 访问时过期检查（LRU策略）

```go
func (c *cache) Get(k interface{}) (interface{}, bool) {
    c.mu.RLock()
    item, found := c.items[k]
    if !found {
        c.mu.RUnlock()
        return nil, false
    }
    
    // 检查是否过期
    if item.Expiration > 0 {
        if time.Now().UnixNano() > item.Expiration {
            c.mu.RUnlock()
            return nil, false  // 过期返回未找到
        }
        // 访问时刷新过期时间（LRU策略）
        item.Expiration = time.Now().Add(c.defaultExpiration).UnixNano()
    }
    
    c.mu.RUnlock()
    return item.Object, true
}
```

**LRU策略优势**：
- **热点数据保护**：频繁访问的数据不会过期
- **自动淘汰**：冷数据自动过期释放内存
- **性能优化**：减少数据库访问次数

### 2.3 批量过期清理

```go
func (c *cache) DeleteExpired() {
    var evictedItems []keyAndValue
    now := time.Now().UnixNano()
    c.mu.Lock()
    
    // 遍历所有缓存项，找出过期的
    for k, v := range c.items {
        if v.Expiration > 0 && now > v.Expiration {
            ov, evicted := c.delete(k)
            if evicted {
                evictedItems = append(evictedItems, keyAndValue{k, ov})
            }
        }
    }
    c.mu.Unlock()
    
    // 调用过期回调函数
    for _, v := range evictedItems {
        c.onEvicted(v.key, v.value)
    }
}
```

**批量处理优势**：
- **减少锁竞争**：一次性处理多个过期项
- **原子操作**：确保数据一致性
- **回调处理**：统一的资源清理机制

## 3. 缓存使用和过期处理

### 3.1 缓存获取逻辑

```go
func (m *CommentManager) getCommentData(topic define.CommentTopic) (*CommentTopicData, error) {
    topicId := utils.PackId(topic.Type, topic.TypeId)
    cache, ok := m.cacheCommentDatas.Get(topicId)  // 自动检查过期

    if ok {
        // 缓存命中，检查任务是否运行
        rd := cache.(*CommentTopicData)
        if rd.IsTaskRunning() {
            return rd, nil
        }
    } else {
        // 缓存未命中（可能过期），从数据库重新加载
        cache = m.commentPool.Get()
        cd := cache.(*CommentTopicData)
        cd.Init(m.r.ID, m.r.rpcHandler)
        err := cd.Load(topic)
        if err != nil {
            m.commentPool.Put(cache)
            return nil, err
        }

        // 设置新的缓存项，过期时间1小时
        m.cacheCommentDatas.Set(topicId, cache, commentCacheExpire)
    }

    return cd, nil
}
```

**处理流程**：
1. **缓存查询**：自动检查过期状态
2. **命中处理**：验证数据有效性
3. **未命中处理**：从数据库重新加载
4. **缓存更新**：设置新的过期时间

### 3.2 延迟加载策略

**优势**：
- **按需加载**：只加载实际需要的数据
- **内存优化**：减少不必要的内存占用
- **启动优化**：避免启动时加载大量数据
- **动态适应**：适应变化的访问模式

## 4. 手动缓存管理

### 4.1 踢出特定缓存

```go
func (m *CommentManager) KickCommentTopicData(topic define.CommentTopic, commentNodeId int32) error {
    if commentNodeId == int32(m.r.ID) {
        // 本节点：直接删除缓存
        topicId := utils.PackId(topic.Type, topic.TypeId)
        cache, ok := m.cacheCommentDatas.Get(topicId)
        if ok {
            cache.(*CommentTopicData).Stop()
            m.cacheCommentDatas.Delete(topicId)  // 手动删除，触发OnEvicted
        }
    } else {
        // 其他节点：通过RPC踢出
        return m.r.rpcHandler.CallKickCommentTopicData(topic, commentNodeId)
    }
    return nil
}
```

**分布式缓存管理**：
- **本地处理**：直接删除本节点缓存
- **远程处理**：通过RPC调用其他节点
- **资源清理**：确保任务停止和对象回收

### 4.2 清空所有缓存

```go
func (m *CommentManager) KickAllCommentTopicData() {
    m.cacheCommentDatas.DeleteAll()  // 删除所有缓存项，触发OnEvicted
}
```

**使用场景**：
- **服务重启**：清理所有缓存状态
- **数据刷新**：强制重新加载所有数据
- **内存清理**：释放大量内存空间

## 5. 资源清理机制

### 5.1 过期回调处理

```go
manager.cacheCommentDatas.OnEvicted(func(k, v any) {
    v.(*CommentTopicData).Stop()    // 停止相关任务
    manager.commentPool.Put(v)      // 放回对象池复用
})
```

**清理步骤**：
1. **停止任务**：避免资源泄漏
2. **对象回收**：放回对象池复用
3. **内存释放**：减少GC压力

### 5.2 对象池复用

```go
// 对象池初始化
manager.commentPool.New = NewCommentData

func NewCommentData() any {
    return &CommentTopicData{}
}
```

**对象池优势**：
- **减少分配**：复用对象，减少内存分配
- **降低GC**：减少垃圾回收压力
- **提高性能**：避免频繁的对象创建销毁

## 6. 性能优化策略

### 6.1 访问时刷新（LRU）

**机制**：每次访问缓存项时，重置过期时间

**优势**：
- **热点保护**：频繁访问的数据不会过期
- **自动淘汰**：冷数据自动过期
- **性能提升**：减少数据库访问

### 6.2 定期清理

**配置**：1分钟清理间隔

**优势**：
- **自动释放**：不再需要的资源自动释放
- **内存控制**：避免缓存无限增长
- **性能稳定**：保持系统性能稳定

### 6.3 惰性检查

**机制**：访问时检查过期状态

**优势**：
- **即时响应**：立即发现过期数据
- **精确控制**：精确的过期时间控制
- **资源节约**：只在需要时检查

## 7. 监控和管理

### 7.1 缓存状态监控

```go
// 获取缓存项数量
func (c *cache) ItemCount() int {
    c.mu.RLock()
    n := len(c.items)
    c.mu.RUnlock()
    return n
}

// 遍历缓存项
func (c *cache) Range(fn func(interface{}) bool) {
    // 遍历所有未过期的缓存项
}
```

### 7.2 缓存管理接口

- **KickCommentTopicData**：踢出特定话题缓存
- **KickAllCommentTopicData**：清空所有缓存
- **ItemCount**：获取缓存项数量
- **Range**：遍历缓存项

## 8. 总结

评论功能的缓存过期处理具有以下特点：

### 8.1 完整性
- ✅ **自动过期**：1小时过期时间，1分钟清理间隔
- ✅ **访问刷新**：LRU策略，访问时重置过期时间
- ✅ **资源清理**：过期时自动停止任务和回收对象
- ✅ **手动管理**：支持手动踢出缓存

### 8.2 高性能
- ✅ **后台清理**：定期清理不影响业务性能
- ✅ **惰性检查**：访问时检查，精确控制
- ✅ **对象池**：减少内存分配和GC压力
- ✅ **批量处理**：减少锁竞争

### 8.3 分布式支持
- ✅ **跨节点管理**：支持分布式缓存踢出
- ✅ **数据一致性**：防止多节点重复缓存
- ✅ **RPC集成**：与微服务架构无缝集成

### 8.4 可维护性
- ✅ **配置化**：过期时间和清理间隔可配置
- ✅ **监控接口**：提供缓存状态监控
- ✅ **优雅停止**：支持优雅的服务停止

这是一个设计完善、功能完整的缓存过期处理系统，能够很好地支持高并发的评论服务需求。

---

*文档生成时间: 2025-01-17*  
*项目: East Eden Game Server - Comment Service*  
*版本: v1.0*
