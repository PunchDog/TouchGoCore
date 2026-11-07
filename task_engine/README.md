# task_engine：任务进度引擎

`touchgocore/task_engine`（包名 `taskengine`）是与业务无关的任务进度/状态机。
它是**库**，没有 Run/Stop 生命周期，不注册进根包 `services.go`。
一个 `*Engine` 绑定一个玩家：`Options.memberID` 决定该实例服务谁，接口上不再传 memberID。

## 对外接口

```go
// 按 parm.Trigger 推进该会员的进行中任务
func (e *Engine) OnEvent(ctx context.Context, parm *ProgressParm) error

// taskID>0 返回单条（1 元素切片）；taskID==0 返回 tasklist 上的可见任务，group 可过滤
func (e *Engine) GetTask(ctx context.Context, taskID int64, group string) ([]*TaskProgress, error)

// 推进指定任务；task 取 GetTask 返回的常驻对象，parm 是本次上报
func (e *Engine) UpdateTask(ctx context.Context, task *TaskProgress, parm *ProgressParm) error

// 全局重载：读取启用定义写入索引，并让每个已注册引擎按同一批定义重建四条链
func (g *Golbal) Reload(ctx context.Context) error

// 按给定定义与会员进度重建四条链（rows 由 taskglobal 统一加载）
func (e *Engine) Reload(ctx context.Context, rows []TaskDef) error

// 释放全部任务节点与自身注册节点；调用后引擎不可再使用
func (e *Engine) Remove()
```

`Engine` 上其余成员（`advance` / `attachLocked` / `defOf` / `rollCycle` 等）全部不导出，
`TestAPISurface` 用反射守住这条边界。没有 `ITask` 接口：任务对象就是 `*TaskProgress`。

## 四条链 = 任务状态

`TaskProgress` 常驻内存并自带四个链表节点（`tasknode / triggernode / completenode / claimednode`），
**节点挂在哪条链上就是任务的当前状态**：

| 状态 | tasklist | triggerlist[trigger] | completetasklist | claimedtasklist |
|---|---|---|---|---|
| active（进行中） | ✅ | ✅ | ❌ | ❌ |
| completed（达成待领取） | ✅ | ❌ | ✅ | ❌ |
| claimed（已结算） | ❌ | ❌ | ❌ | ✅ |

- `Reload` 加载数据时按库中状态入链（`attachLocked`）：进 claimed 链的任务另外三条链都不进；
  进 complete 链的任务不进 triggerlist。
- `OnEvent` 只从 `triggerlist[trigger]` 取候选，因此已完成/已结算的任务天然不再被事件推进。
- 达标后 `advance` 调 `relocate` 做**匹配切换**：CLAIM 摘 trigger 挂 complete（tasklist 保留），
  AUTO 摘 trigger+task 挂 claimed。
- 跨周期由 `rollCycle` 兜底：`OnEvent/GetTask/UpdateTask` 入口先扫 complete/claimed 两条链，
  `CycleKey` 已过期的任务清零复位回 active 并重新入链。
- 节点在四条链之间迁移只走 `Detach` + `Add`：`Detach` 摘链但保留 `data`、不归还对象池，
  节点可重新挂到别的链；`Remove`（清空 `data` 并回池）只用于 `Remove()` 收尾。
- 节点用 `list.NewNode(t, nil)` 走对象池分配：存活期间不调用 `Remove`，不会被池复用改写；
  `Remove()` 摘掉全部任务节点与引擎自身节点，任务节点因此回到 `sync.Pool`
  （引擎是内嵌 `list.Node`，不属于池分配，只能摘链）。

进程级共享状态集中在 `global.go` 的 `Golbal` 结构体（实例 `taskglobal`）：

| 字段 | 用途 |
|---|---|
| `defs syncmap.Map[int64, *TaskDef]` | 任务定义索引（只存启用行） |
| `engineList NodeList` | 已创建引擎的注册链（`Engine` 内嵌 `list.Node`） |

`taskglobal.Reload` 用 `loadDefs` 读取启用行并整体替换索引，再广播给每个已注册引擎；
`New` 也经 `taskglobal.loadDefs` 取同一批 rows 后调 `e.Reload(ctx, rows)`。
`TaskProgress.GetDef()` 查 defs，未命中由 `defOf` 经 `CacheQuery` 回源后 `taskglobal.storeDef` 回填；
`New` 调 `taskglobal.addEngine` 注册引擎。

## ProgressParm 与进度口径

```go
type ProgressParm struct {
    Trigger            string
    ProgressParmInvite int64 // 全局数据覆盖
    ProgressParmAdd    int64 // 累计增量
}
```

| `TaskDef.ProgressType` | 进度计算 |
|---|---|
| `INVITE` | `progress = ProgressParmInvite`（覆盖） |
| `ADD` | `progress += ProgressParmAdd`（累加） |
| 空/未知 | `Invite != 0` 则覆盖，否则累加 `Add` |

完成判定：按 `TaskDef.TargetCountType`（`>` / `=` / `<`，空按 `=`）与 `TargetCount` 比较；
`TargetCount<=0` 按 1。达标时 `AUTO` 写 `claimed`、`CLAIM` 写 `completed`；未达标写 `active`。
终态不再降级，重复推进返回 `ErrAlreadyClaimed`。

## 用法

```go
tasks, _ := engine.GetTask(ctx, 0, "")
for _, tk := range tasks {
    if err := engine.UpdateTask(ctx, tk, &taskengine.ProgressParm{ProgressParmAdd: 1}); err != nil { ... }
}
```

`UpdateTask` 传副本也可以：按 `TaskID` 换回常驻对象后再推进，否则节点摘挂作用不到索引上。

## 发奖（rewardFn）

发奖逻辑在宿主侧，由 `Options.rewardFn func(*TaskProgress)` 注入：

- 仅当任务 `GrantMode == AUTO` 且本次判定达标时回调一次，`OnEvent` 与 `UpdateTask` 两条路径一致；
- 回调入参是即将落库的任务对象，可用 `GetDef().Reward` 读 `reward_json`；
- 顺序为**先发奖后落库**；回调没有返回值，发奖失败不会阻断写入，写入失败也可能已经发奖——
  宿主必须自带幂等；
- `CLAIM` 达标只落 `completed`，引擎不回调，等宿主发放；
- 终态任务不再被推进，故不会重复发奖；
- 回调在 `e.mu` 与事务行锁内执行，禁止在回调里再次调用引擎处理同一任务。

## 完成通知

`CompletedNotifyTrigger` 是固定触发点常量：达标后以 `ProgressParmAdd=1` 再扇出一次 `OnEvent`，
递归深度上限 8。把任务的 `trigger` 配成 `CompletedNotifyTrigger` 即可接收完成通知；
没有任何任务挂在该触发点时不产生额外写入。

## 快速接入

```go
// 1. 统一缓存层必须先初始化（Redis 一级 + 定时回写 DB）
if err := db.InitCache(db.CacheConfigFrom(app.Cfg.Cache), app.Redis.Get()); err != nil { ... }

// 2. 把 task_def / task_progress 绑定到缓存层
if err := taskengine.OpenStorage(app.MySQL); err != nil { ... }

// 3. 每个会员一个引擎
engine, err := taskengine.New(taskengine.Options{
    memberID: uid,  // 见「已知缺口」
    rewardFn: func(p *taskengine.TaskProgress) { /* 按 p.GetDef().Reward 发放 */ },
})
```

`New` 会立即 `Reload`（读 `task_def` + 该会员 `task_progress`），缓存层未初始化时构造失败。
建表见 `task_engine/schema.sql`；历史库需先执行 `task_engine/migrate_unique_progress.sql`
补三列唯一键。`CreateDef`/`UpdateDef` 是包级存储原语，入参都是完整的 `TaskDef`：
`CreateDef` 要求调用方给出非零 `id`（行键由主键组成），`UpdateDef` 先读旧行再全量写回；
两者都走 `CacheSave(mustSave=true)`，返回前已落库。

## 缓存与落库

- 读：`CacheQuery`。按 (member_id, task_id, cycle_key) 点查先读 Redis，未命中回源 DB 并回填；
  定义按 id 点查、启用列表查 DB（列表查询不走 Redis）。
- 写：`CacheSave(mustSave=false)`。进度写只更新 Redis 并登记脏账，由缓存层定时批量落库，
  默认滞后约一个 `flush_interval_ms`（3s）；停机时 `StopCache` 做 final flush。
- 落库依赖 `task_progress(member_id, task_id, cycle_key)` 唯一键，走
  `INSERT ... ON DUPLICATE KEY UPDATE`，每个周期严格一行。
- 内存对象与 Redis 里是同一份进度；进程崩溃最多丢失最后一个 flush 周期。

## 关键语义

1. `OnEvent` 两阶段：先按事件前的状态确定候选，再统一推进，同事件不连锁解锁前置。
2. 前置就绪判定优先看常驻前置任务的链归属（终态即完成），未常驻才查库。
3. 投放窗口在 `UpdateTask`、`OnEvent` 与 `GetTask` 三侧都拦截。
4. 周期片由 `TaskCycleKey(cycle, now, taskID)` 生成；旧周期进度行不会被合并进本周期的常驻对象。
5. 时间源统一 `util.CurrentTime()`（业务/虚拟时间）。
6. `ClearOnCycle` / `NextDayClaim` / `CycleLimit` 保留在定义中，引擎不再消费。

## 已知缺口

- `Options.memberID` 与 `Options.rewardFn` 目前是**未导出**字段，包外宿主无法装配，
  只能在包内（或测试）构造；导出前本包不具备对外可用性。
- 引擎按「每会员一个 Engine」设计且任务对象常驻内存：装配层要自己持有/回收这批实例，
  在线人数即内存任务数。
- `Remove()` 调用后引擎不可再使用；调用前必须确保该会员没有并发的事件或查询
  （摘链会立即生效，不与 Range 快照对齐）。
- 任务对象的进度字段在摘挂时受 `e.mu` 保护，但 `GetTask` 读字段不加锁；
  同一会员的事件必须串行投递（单逻辑线程）。

## 存储注意事项

- `task_progress` 必须有 `(member_id, task_id, cycle_key)` 唯一键，否则批量落库的
  `ON DUPLICATE KEY UPDATE` 不触发，会插入重复行。
- 进度写是延迟落库（`mustSave=false`）：读到的是 Redis 最新值，DB 可能滞后一个 flush 周期；
  需要强一致的外部读取应先 `db.FlushCache(ctx, "task_progress")`。
- 定义表也纳入缓存（TTL 跟随全局配置，默认不过期）：绕过 `UpdateDef` 直接改库，
  Redis 里的旧定义在键过期前不可见。定义变更统一走 `UpdateDef` + `taskglobal.Reload`。
- `CacheSave` 是全行覆盖：调用方必须持有完整行（`UpdateDef` 会先读旧行合并）。
- `defs` 与四条链都是单进程视图；多实例部署时任务定义变更需各自 `Reload`。
- 运行集成测试需提供真实 Redis/MySQL（见 `storage_test.go` 中的 `TASKENGINE_TEST_*` 环境变量）。
