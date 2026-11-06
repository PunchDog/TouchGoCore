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

// 重载定义与会员进度，按状态重建四条链
func (e *Engine) Reload(ctx context.Context) error
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
- 节点摘挂必须走 `nodeFor`：`list.Node` 在 `Remove()` 时会清空 `data`（对象池语义），
  摘过链的旧节点不能再直接挂回去，否则链上会留下 `data` 为空的幽灵节点。
- 节点用 `list.NewNode(t, &list.Node{})` 创建，**不传 nil**：池分配节点 `Remove` 后会被复用改写。

进程级共享状态集中在 `global.go` 的 `Golbal` 结构体（实例 `taskglobal`）：

| 字段 | 用途 |
|---|---|
| `defs syncmap.Map[int64, *TaskDef]` | 任务定义索引（只存启用行） |
| `engineList NodeList` | 已创建引擎的注册链（`Engine` 内嵌 `list.Node`） |
| `notifytrigger Policy` | 完成通知触发点 |

`Reload` 用 `taskglobal.setDefs` 整体替换定义索引；`TaskProgress.GetDef()` 查它，
未命中由 `defOf` 回源 DB 后 `taskglobal.storeDef` 回填；`New` 调 `taskglobal.addEngine` 注册引擎。

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

`SetCompletedNotifyTrigger(Policy)` / `CompletedNotifyTrigger()` 读写全局完成通知触发点
（存在 `taskglobal.notifytrigger`）：达标后以 `ProgressParmAdd=1` 再扇出一次 `OnEvent`，
递归深度上限 8。**零值（空串）即关闭**，装配层启动时设置。

## 快速接入

```go
store := taskengine.NewMySQLStore(app.MySQL)

// 可选：进度写线走两级缓存（Redis 一级 + 定时回写 DB）
progressCache, err := taskengine.OpenProgressCache(app.Cache, store)
if err != nil { ... }

taskengine.SetCompletedNotifyTrigger("TASK_COMPLETED") // 不需要就留空

engine, err := taskengine.New(taskengine.Options{
    Store:    store,
    Cache:    progressCache, // nil = 直接走 Store 事务
    memberID: uid,           // 见「已知缺口」
    rewardFn: func(p *taskengine.TaskProgress) { /* 按 p.GetDef().Reward 发放 */ },
})
```

`New` 会立即 `Reload`（读 `task_def` + 该会员 `task_progress`），Store 不可用时构造失败。
建表见 `task_engine/schema.sql`。`MySQLStore` 另外提供 `CreateDef`/`UpdateDef` 两个存储原语。

## 缓存与落库

- `OpenProgressCache(layer, store, opts...)` 在 `app.Cache` 上注册 `task_progress` 类型；
- `Reload` 读线：缓存路径逐任务 `GetOrLoad`，回退路径一次 `ListProgressByMember`；
- 写线 `persist`：缓存路径 `Cache.Write`（同步写 Redis，框架定时回写 `SaveProgress`）；
  未配置缓存时走 `Store.WithTx` + `UpsertProgress`；
- 内存对象与缓存里是同一份进度，崩溃时以最后一次 `persist` 为准。

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
- 任务对象的进度字段在摘挂时受 `e.mu` 保护，但 `GetTask` 读字段不加锁；
  同一会员的事件必须串行投递（单逻辑线程）。

## 存储注意事项

- `UpsertProgress` 前先 `LoadProgress` 定位 id，再 `WHERE id=? FOR UPDATE`；直接
  `ORDER BY id DESC LIMIT 1 FOR UPDATE` 会在 `status` 二级索引上死锁（1213）。
- 无行路径不加 `FOR UPDATE`（gap lock 会与并发 INSERT 互相等 insert intention）。
- `defs` 与四条链都是单进程视图；多实例部署时任务定义变更需各自 `Reload`。
