// Package websocket 网关连接层：升级、鉴权、按类名派发消息回调。
//
// # 装配顺序
//
// RegisterCall 在 Run 之前完成（Run 时按类名从对象池取回调实例，注册晚了连接会报
// 「未找到类名对应的ICall接口实现」）；Run(ctx) 起常驻的 Tick 消费协程，Stop(ctx) 收。
// 一轮 Run 的参数（含 Worker 池配置）在该轮生效，重 Run 会重建。
//
// # 并发模型：默认单线程，Worker 池是显式开启的例外
//
// 框架的常态假设是「业务回调只跑在一个协程上」，这个假设是否成立完全取决于
// config.ws.worker_pool_size：
//
//   - worker_pool_size = 0（默认，串行模式）：所有连接的 OnMessage 都在 Tick 这唯一
//     的消费协程上依次执行。此时业务回调内部读写自己那份跨连接状态可以不加锁，
//     消息处理顺序即全局到达顺序。代价是一条慢回调会堵住全部连接。
//   - worker_pool_size > 0（并行模式）：Tick 只做派发，OnMessage 由 N 个常驻 Worker
//     并发调用。跨连接一定并发；同一连接是否保序取决于 shard_by_key：
//     true 时按 uid 固定到同一个 Worker，片内保序；false 时轮询派发，跨消息不保证
//     任何顺序。两种情况下顺序都可能被丢弃打断（读队列满丢弃、Worker 队列满兜底丢弃，
//     计数见 WorkerPoolFullCount）。
//
// OnConnect 跑在升级 HTTP 请求的那个协程上，OnClose 跑在发起关闭的那个协程上
// （读协程退出、心跳超时、或 Stop 的遍历），无论是否启用 Worker 池，它们都与
// OnMessage 并发。换句话说：即使串行模式下，「连接级生命周期回调」与「消息回调」
// 也是两个协程，跨这两处的共享状态始终需要锁。
//
// # 已经线程安全的部分
//
// 客户端表（clientMap，单锁表或分片表）、连接与消息统计（atomic）、Worker 池指针、
// 鉴权函数（authMu）都由本包自己上锁，宿主直接调 GetClient / SendMsg / 统计接口
// 不需要额外同步。
//
// # 需要宿主自己保证的部分
//
//   - 业务自建的跨连接状态（uid→房间、uid→会话一类的全局 map）：并行模式下必然被
//     多协程并发访问，必须自带锁。写法见 ExampleRegisterCall。
//   - 连接级状态（回调结构体自己的字段）只在「同一连接的回调不并发」时才不用锁：
//     串行模式与 worker_pool_size>0 + shard_by_key=true 成立；轮询派发
//     （shard_by_key=false）下同一条连接的两条消息可以同时在两个 Worker 上执行，
//     这时连连接级字段也要锁，或者由业务自己把它转交给单协程处理。
//   - 回调实例是池化的：RegisterCall 注册的构造函数产出的实例在连接关闭后会被归还，
//     下一位主人认领到的是带着上一份状态的对象。所以回调结构体的字段必须在 OnConnect
//     里显式复位，不能依赖零值。
//   - *Client 同样是池化的，OnClose 返回之后不得再持有它或它的引用（含闭包捕获），
//     否则读到的是下一个连接的字段。需要在连接关闭后仍然可用的信息，应当在此之前
//     取值复制出来。
//   - 分客户端表（UseShardedClientMap）的 Range 是逐片快照，不保证跨片的全局一致顺序。
package websocket
