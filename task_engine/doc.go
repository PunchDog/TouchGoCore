// Package taskengine 提供与具体业务无关的任务进度引擎。
//
// 一个 Engine 绑定一个玩家（Options.MemberID），对外入口：
//
//	OnEvent    按 ProgressParm.Trigger 推进该会员进行中任务；
//	GetTask    查询单个任务（taskID>0）或任务列表（taskID==0），返回常驻的 *TaskProgress；
//	UpdateTask 按传入的 ProgressParm 推进指定任务；AUTO 达标先回调 Options.RewardFn 再落 claimed；
//	Reload     按全局加载的定义与会员进度重建链表归属；
//	Remove     释放全部任务节点与自身注册节点，调用后引擎不可再使用。
//
// 内存模型：每个启用任务在引擎里常驻一个 *TaskProgress，带四个链表节点，
// 节点挂在哪条链上就是任务的当前状态——
//
//	tasklist         全部可见任务（claimed 除外）
//	triggerlist      按触发点分组，只挂「进行中」任务；OnEvent 从这里取候选
//	completetasklist 已达成待领取（completed），不进 triggerlist
//	claimedtasklist  已结算（claimed），只在这一条链上
//
// Reload 加载数据时按库中状态入链；OnEvent/UpdateTask 判定达标后就地切换归属
// （摘掉 trigger/task/claimed 节点，改挂 complete 或 claimed 节点）。跨周期时
// 终态链上的任务由 rollCycle 复位回进行中。
//
// 进程级共享状态都在 global.go 的 Golbal 结构体里（实例为 taskglobal）：
// defs 定义索引、engineList 引擎注册表与完成通知配置。
// taskglobal.Reload 把启用行整体写入 defs 并广播给各引擎，索引未命中时按 id 回源
// 缓存门面并回填；TaskProgress.GetDef 即查这张表。New 会把引擎挂进 engineList。
//
// 进度口径由 TaskDef.ProgressType 决定：INVITE 用 ProgressParmInvite 覆盖，
// ADD 用 ProgressParmAdd 累加；与 TargetCount 按 TargetCountType（> / = / <，空按 =）
// 比较，达成即完成。
//
// 发奖由宿主注入的 Options.RewardFn（类型 RewardFn）完成：只有 AUTO 达标时引擎回调一次，
// 入参是即将落库的任务对象与框架显式构造的 RewardKey（MemberID / TaskID / CycleKey）。
// 宿主必须以 key.String() 作为唯一去重键并保证回调幂等；CLAIM 只落 completed，
// 等宿主自行发放。完成通知的触发点是固定常量 CompletedNotifyTrigger。
// 回调没有返回值、在 e.mu 与事务行锁内执行，见 reward.go 与 README「发奖」。
//
// 存储读写统一走 touchgocore/db 的缓存门面：读用 CacheQuery、写用 CacheSave，
// 包内不区分 Redis 与数据库。进度写用 mustSave=false（延迟落库），只允许用于可丢失的
// 任务进度；资金 / 账本列禁止该路径，硬约束见 db 包注释。使用前必须先 db.InitCache
// 并调用 OpenStorage 把 task_def / task_progress 绑定到统一缓存层。
//
// 本包是库，不是 Service：没有 Run/Stop 生命周期，由调用方在装配层构造 *Engine 并持有。
package taskengine
