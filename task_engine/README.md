# task_engine：任务进度引擎

`touchgocore/task_engine`（包名 `taskengine`）是与业务无关的任务进度/状态机。
它是**库**，没有 Run/Stop 生命周期，不注册进根包 `services.go`。
一个 `*Engine` 绑定一个玩家：导出的 `Options.MemberID` 决定该实例服务谁，接口上不再传 memberID；
`Options.RewardFn` 由包外宿主直接注入。

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

## 发奖（Options.RewardFn）

发奖逻辑在宿主侧，由导出的 `Options.RewardFn` 注入：

```go
type RewardKey struct {
    MemberID int64
    TaskID   int64
    CycleKey string
}
func (k RewardKey) String() string // "<MemberID>:<TaskID>:<CycleKey>"

type RewardFn func(ctx context.Context, key RewardKey, p *TaskProgress)
```

- 仅当任务 `GrantMode == AUTO` 且本次判定达标时回调一次，`OnEvent` 与 `UpdateTask` 两条路径一致；
- `key` 由框架从 `p.MemberID / p.TaskID / p.CycleKey` 显式构造并盖章，`p` 是即将落库的任务对象，
  可用 `p.GetDef().Reward` 读 `reward_json`；
- 顺序为**先发奖后落库**；回调没有返回值，发奖失败不会阻断写入，写入失败也可能已经发奖；
- `CLAIM` 达标只落 `completed`，引擎不回调，等宿主发放；
- 终态任务不再被推进，故不会重复发奖；
- 回调在 `e.mu` 与事务行锁内执行，`ctx` 供宿主设置超时 / 取消。

**接入清单（硬项，缺一不可）**

1. **以 `key.String()` 为唯一去重键**：发奖流水 / 去重表按 `<MemberID>:<TaskID>:<CycleKey>`
   建唯一键。框架现在提供这个幂等键，宿主不要再自己从 `p` 里另拼一套规则。
2. **回调必须幂等**：同一 `key.String()` 重复进入时不得重复发放。回调刻意不返回 `error`——
   `applyProgress` 在回调前已把 `p.Status` 置为 `claimed`，若让回调阻断落库，会造出
   「内存终态 + DB 未落库 + 不再重发奖 + 丢进度」的新不一致；失败由宿主按 `key` 重试补偿。
3. **禁止在回调里再次调用引擎处理同一任务**：回调仍在 `e.mu` 与事务行锁内，重入会死锁或重复推进。
4. **远程发奖必须设超时**：发奖 RPC 在锁内执行，会拉长锁持有时间（**已知风险**，本轮未把回调
   移出锁）；用 `ctx` 施加超时 / 取消，避免拖住该会员的全部事件。

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

// 3. 每个会员一个引擎（示例需 import "context"）
engine, err := taskengine.New(taskengine.Options{
    MemberID: uid,
    RewardFn: func(ctx context.Context, key taskengine.RewardKey, p *taskengine.TaskProgress) {
        // 以 key.String() 幂等去重，再按 p.GetDef().Reward 发放；ctx 用于超时 / 取消
        grantOnce(ctx, key.String(), p.GetDef().Reward)
    },
})
```

`Options.MemberID` / `Options.RewardFn` 均为导出字段，包外宿主可直接装配。
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
- `mustSave=false` 只允许用于可丢失的任务进度 / 统计类；资金 / 账本列的唯一合法写路径是
  `db.Increment` / `db.CompareAndSwap`（同步落库），硬约束见 `db/cache_api.go` 与根 `README.md`。

## 关键语义

1. `OnEvent` 两阶段：先按事件前的状态确定候选，再统一推进，同事件不连锁解锁前置。
2. 前置就绪判定优先看常驻前置任务的链归属（终态即完成），未常驻才查库。
3. 投放窗口在 `UpdateTask`、`OnEvent` 与 `GetTask` 三侧都拦截。
4. 周期片由 `TaskCycleKey(cycle, now, taskID)` 生成；旧周期进度行不会被合并进本周期的常驻对象。
5. 时间源统一 `util.CurrentTime()`（业务/虚拟时间）。
6. `ClearOnCycle` / `NextDayClaim` / `CycleLimit` 保留在定义中，引擎不再消费。

## 已知缺口

- 引擎按「每会员一个 Engine」设计且任务对象常驻内存：装配层要自己持有/回收这批实例，
  在线人数即内存任务数。
- `Remove()` 调用后引擎不可再使用；调用前必须确保该会员没有并发的事件或查询
  （摘链会立即生效，不与 Range 快照对齐）。
- 任务对象的进度字段在摘挂时受 `e.mu` 保护，但 `GetTask` 读字段不加锁；
  同一会员的事件必须串行投递（单逻辑线程）。
- `Options.RewardFn` 在 `e.mu` 与事务行锁内执行，远程发奖会拉长锁持有时间；
  若要把回调移出锁，须同时处理「先发奖后落库」的顺序语义，属行为变更。

## 存储注意事项

- `task_progress` 必须有 `(member_id, task_id, cycle_key)` 唯一键，否则批量落库的
  `ON DUPLICATE KEY UPDATE` 不触发，会插入重复行。
- 进度写是延迟落库（`mustSave=false`）：读到的是 Redis 最新值，DB 可能滞后一个 flush 周期；
  需要强一致的外部读取应先 `db.FlushCache(ctx, "task_progress")`。
- 定义表也纳入缓存（TTL 跟随全局配置，默认不过期）：绕过 `UpdateDef` 直接改库，
  Redis 里的旧定义在键过期前不可见。定义变更统一走 `UpdateDef` + `taskglobal.Reload`。
- `CacheSave` 是全行覆盖：调用方必须持有完整行（`UpdateDef` 会先读旧行合并）。
- `defs` 与四条链都是单进程视图；多实例部署时任务定义变更需各自 `Reload`。
- 集成测试的环境变量、专用库要求与破坏性操作见下节「集成测试门禁」。

## 集成测试门禁（TASKENGINE_TEST_*）

本包**没有 in-memory 假存储**：`db.InitCache` 固定 Redis store、`CacheBindMySQL` 固定真实
MySQL 仓储、所有数据接口固定进程级全局单例、`task_engine` 直调 `db.Cache*` 包级函数，
公开面没有一处可插拔点。因此行为用例只能在**专用隔离库**上真跑；不要尝试重建旧式假存储
（可行性判定见 `deliverables/software-company/fix-plan-2026-11-09.md` §2）。

### 环境变量（7 个，全部必填，运行时注入）

| 变量 | 含义 | 约束 |
|---|---|---|
| `TASKENGINE_TEST_MYSQL_HOST` | MySQL 地址 `host:port` | 指向专用隔离库 |
| `TASKENGINE_TEST_MYSQL_USER` | MySQL 用户名 | — |
| `TASKENGINE_TEST_MYSQL_PASSWORD` | MySQL 密码 | 可为空串；值不写入仓库任何文件 |
| `TASKENGINE_TEST_MYSQL_DB` | MySQL 库名 | **必须含 `test`**（`storage_test.go` 护栏） |
| `TASKENGINE_TEST_REDIS_ADDR` | Redis 地址 | 指向专用隔离 Redis DB |
| `TASKENGINE_TEST_REDIS_PASSWORD` | Redis 密码 | 可为空串 |
| `TASKENGINE_TEST_REDIS_DB` | Redis 库编号 | 整数，缺省 0 |

> 凭据只由运行环境注入：`bench/run.ps1 -Integration` **只校验这 7 个变量是否已定义**，
> 不保存、不硬编码任何值；缺任一个即显式报错退出，绝不允许静默退回全 SKIP。
> 密码允许空串、`REDIS_DB` 允许 0，所以判据是「变量是否存在」而不是「值是否非空」。
> `db/dbcache` 的事务层真库测试复用同一组变量。
> **Windows PowerShell 5.1 下 `$env:X = ""` 会直接删除该变量**，空密码无法表示成
> 「已定义但为空」；需要空值时请改用 pwsh（PowerShell 7）或 CI 注入，不要用 PS 5.1 伪造空值。

### 破坏性操作（务必先读）

- `setupStorage` → `initStorage` → `recreateSchema` 会
  **`DROP TABLE IF EXISTS task_progress / task_def` 后按 `schema.sql` 重建**——
  是 **DROP TABLE 后重建，不是 TRUNCATE**，目标库里同名表的数据会全部消失。
- 每个用例首次进入时 `resetStorage` 还会 `TRUNCATE TABLE task_progress / task_def`
  并清空 `*task_*` 的 Redis 键。
- 库名必须含 `test`：`storage_test.go` 的护栏对其它库名直接 `Fatal`，防止误连业务库。
- **因此这 7 个变量必须指向专用隔离 MySQL 库与专用 Redis DB，严禁指向业务库或任何有数据的库。**

### 跑法

```powershell
# 先由运行环境注入上面 7 个变量（不要写进任何文件），再：
powershell -File bench/run.ps1 -Integration
# 或单包直跑：
go test -timeout 600s -count=1 -v ./task_engine/
```

`-count=1` **必需**：不加时同包重复运行会命中测试缓存，「绿」可能只是上一次的结果。
环境变量未配齐时 `go test` 会把依赖存储的用例整组 `Skip`，**SKIP 不等于通过**；
门禁判据是 `--- SKIP:` 行数 = 0、`--- FAIL:` 行数 = 0、真跑用例数 ≥ 35。
`bench/run.ps1 -Integration` 的输出写 `docs/taskengine-integration.txt`
（`docs/` 被 `.gitignore:38` 忽略，属本地产物，不入库）。
`localtimer` 是时序敏感包，连跑 3 次的约定仍然适用（见 `docs/testing-conventions.md`）。

> 警告：`git clean -xfd` 会清掉本地所有被忽略的 `p0_`~`p3_` 评审轮次测试资产。
> 这些文件 untracked 且被 `.gitignore` 忽略，清理后 git 无法恢复；清理前必须先把它们
> 归档到仓库外的目录（发布策略与命名约定见根 `README.md` 的「开发」段）。
