// Package taskengine 提供与具体业务无关的任务进度引擎。
//
// 一个 Engine 绑定一个玩家（Options.memberID），对外入口：
//
//	OnEvent    按 ProgressParm.Trigger 推进该会员进行中任务；
//	GetTask    查询单个任务（taskID>0）或任务列表（taskID==0），返回常驻的 *TaskProgress；
//	UpdateTask 按传入的 ProgressParm 推进指定任务；AUTO 达标先回调 rewardFn 再落 claimed；
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
// defs 定义索引、engineList 引擎注册表、store 定义读取源与完成通知配置。
// taskglobal.Reload 把启用行整体写入 defs 并广播给各引擎，索引未命中时按 id 回源
// DB 并回填；TaskProgress.GetDef 即查这张表。New 会把引擎挂进 engineList。
//
// 进度口径由 TaskDef.ProgressType 决定：INVITE 用 ProgressParmInvite 覆盖，
// ADD 用 ProgressParmAdd 累加；与 TargetCount 按 TargetCountType（> / = / <，空按 =）
// 比较，达成即完成。
//
// 发奖由宿主注入的 Options.rewardFn 完成：只有 AUTO 达标时引擎回调一次，
// 回调入参是即将落库的任务对象；CLAIM 只落 completed，等宿主自行发放。
// 完成通知由 SetCompletedNotifyTrigger 控制，零值关闭。
//
// 进度写线可接入 touchgocore/db 两级缓存（OpenProgressCache + Options.Cache），
// 未配置缓存时走 Store 事务 upsert。
//
// 本包是库，不是 Service：没有 Run/Stop 生命周期，由调用方在装配层构造 *Engine 并持有。
package taskengine
