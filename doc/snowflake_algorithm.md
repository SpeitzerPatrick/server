# 雪花算法 (Snowflake)

雪花算法是一种分布式系统中生成唯一ID的算法，最初由Twitter开发并开源。在我们的项目中，使用了Sony的Sonyflake实现，这是雪花算法的一个变种。

## 基本原理

雪花算法生成的ID是一个64位的长整数，由以下部分组成：

1. **时间戳部分**：占据ID的高位，通常是毫秒级时间戳
2. **工作机器ID部分**：标识不同的服务器或服务实例
3. **序列号部分**：同一毫秒内的自增序列，确保同一时间戳下生成的ID唯一

## Sonyflake实现

在我们的代码中使用的Sonyflake结构如下：

```go
// Sonyflake ID组成
// 39 bits for time in units of 10 msec
//  8 bits for a sequence number
// 16 bits for a machine id
```

具体来说：
- 39位时间戳：以10毫秒为单位，可使用约174年
- 8位序列号：每10毫秒内可生成256个ID
- 16位机器ID：最多支持65536个节点

## 在项目中的应用

我们的项目中通过`utils/snow_flakes.go`封装了Sonyflake的使用：

```go
// snow flakes machine_id: 10 bits machineID + 6 bits plugin_type
func InitMachineID(machineID int16, startTime int64, cb func()) {
    sfs.cb = cb
    sfs.once.Do(func() {
        for n := 0; n < define.SnowFlake_End; n++ {
            var st sonyflake.Settings

            st.MachineID = func() (uint16, error) {
                newID := uint16(machineID<<6) + uint16(n)
                return newID, nil
            }

            st.CheckMachineID = func(id uint16) bool {
                return id <= (1<<16 - 1)
            }

            st.StartTime = time.Unix(startTime, 0)

            sf := sonyflake.NewSonyflake(st)
            if sf == nil {
                log.Panic().Str("start_time", st.StartTime.String()).Msg("sonyflake not created")
            }

            sfs.ids = append(sfs.ids, sf)
        }
    })
}
```

我们的实现有以下特点：
1. **机器ID分配**：将16位机器ID分为两部分
   - 高10位：表示实际的机器ID
   - 低6位：表示插件类型（如账号、玩家、邮件等）

2. **ID生成**：通过`NextID`函数生成唯一ID
   ```go
   func NextID(tp int) (int64, error) {
       if tp < 0 || tp >= define.SnowFlake_End {
           return -1, fmt.Errorf("wrong id generated, type:%d", tp)
       }

       id, err := sfs.ids[tp].NextID()
       if err == nil {
           sfs.cb()
       }

       return int64(id), err
   }
   ```

3. **ID解析**：提供函数解析ID中的机器ID部分
   ```go
   func MachineID(id int64) int16 {
       m := sonyflake.Decompose(uint64(id))
       return int16(m["machine-id"])
   }

   func MachineIDHigh(id int64) int16 {
       return MachineID(id) >> 6
   }

   func MachineIDLow(id int64) int16 {
       return MachineID(id) & 15
   }
   ```

## 雪花算法的优势

1. **高性能**：纯内存操作，无需数据库交互
2. **去中心化**：不依赖中央服务器，每个节点独立生成ID
3. **有序性**：生成的ID大致按时间递增，便于索引和排序
4. **可追溯**：ID中包含时间和机器信息，便于问题排查
5. **高可用**：无单点故障风险

## 在各服务中的应用

各微服务通过`initSnowflake`方法初始化雪花算法：

```go
func (s *Service) initSnowflake() {
    store.GetStore().AddStoreInfo(define.StoreType_Machine, "machine", "_id")
    if err := store.GetStore().MigrateDbTable("machine"); err != nil {
        log.Fatal().Err(err).Msg("migrate collection machine failed")
    }

    err := store.GetStore().FindOne(context.Background(), define.StoreType_Machine, s.ID, s)
    if err != nil && !errors.Is(err, store.ErrNoResult) {
        log.Fatal().Err(err).Msg("FindOne failed when Service.initSnowflake")
    }

    utils.InitMachineID(s.ID, s.SnowflakeStartTime, func() {
        s.SnowflakeStartTime = time.Now().Unix()
        err := store.GetStore().UpdateOne(context.Background(), define.StoreType_Machine, s.ID, s)
        _ = utils.ErrCheck(err, "UpdateOne failed when NextID", s.ID)
    })
}
```

这确保了各服务能生成全局唯一的ID，即使在分布式环境中也不会冲突。

## 使用示例

以下是在项目中生成唯一ID的示例：

```go
// 生成玩家ID
playerID, err := utils.NextID(define.SnowFlake_Player)
if err != nil {
    log.Error().Err(err).Msg("Failed to generate player ID")
    return
}

// 生成邮件ID
mailID, err := utils.NextID(define.SnowFlake_Mail)
if err != nil {
    log.Error().Err(err).Msg("Failed to generate mail ID")
    return
}

// 从ID中提取机器ID
machineID := utils.MachineID(playerID)
```

## 注意事项

1. 确保各服务的机器ID唯一，避免ID冲突
2. 服务器时间必须准确，建议使用NTP同步
3. 启动时间（StartTime）一旦设置不应更改，否则可能导致ID重复
4. 在高并发场景下，需要注意序列号溢出问题