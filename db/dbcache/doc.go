// Package dbcache 提供 Redis 一级缓存 + 数据库定时回写的统一读写门面。
//
// 对外数据接口为 Query（条件查询）、Save（全值写入）与 Delete（物理删除）。
// Redis 采用「一行一 hash」：{prefix:table}:r:<key>，field 为列名，value 为列值
// 字符串；延迟落库用同表的脏 ZSET 记录待写主键，每表一个周期定时器批量回写数据库。
// 删除用 {prefix:table}:t:<key> 墓碑阻挡在途脏账把行写回来。
//
// # 读写路径
//
//   - 单条且条件恰好覆盖全部键列：先读 Redis，命中直接返回；未命中回源数据库，
//     回填后从 Redis 读回并返回。
//   - 列表或含非键列的查询：直接读数据库，命中行回填 Redis，返回数据库结果。
//   - Save(mustSave=false)：写 Redis 全字段 + 刷新脏 ZSET 到期时间，由周期定时器落库。
//   - Save(mustSave=true)：写 Redis 后立即落库；落库失败保留脏账并返回错误。
//   - Delete(cond)：条件必须恰好覆盖全部键列，否则返回 ErrPartialKey；顺序为
//     立墓碑 → 删数据库 → 清 Redis 行 hash / 脏账 / 重试账。数据库删除失败时
//     回滚墓碑并保留 Redis 内容。
//
// # 已知限制
//
//  1. 列表查询不走 Redis：行 hash 没有二级索引，无法可靠判断列表命中。
//  2. 条件只支持等值与 IN（指针字段 / 指针切片），不支持范围、排序、分页；
//     单行回源可用 BindOption.OrderBy。
//  3. 非键条件返回数据库行，可能与 Redis 中尚未落库的新值不一致；强一致场景
//     使用 mustSave=true 或先 Flush。
//  4. TTL 默认关闭，Redis 内存随表增长，需要运维控容量。
//  5. Redis 宕机或未开 AOF 会丢失脏 ZSET；生产必须开启 appendonly everysec。
//  6. Mongo 没有批量 upsert，落库逐行执行，且 DbOperate 内部固定 5s 超时；
//     Mongo 模型的多单词字段必须显式写 bson tag，否则缓存列名与集合字段名不一致。
//  7. 跨进程并发 flush 只保证幂等 upsert 与不误删新脏账，不保证写序。
//  8. *string 指向空串与 nil 在 hash 中同为 ""，读回为 nil。
//  9. 删除与「已 Claim 未 upsert」的在途批次并发时，该批次仍可能把行整行写回；
//     窗口等于一次批量落库耗时，下一轮 Query 回源后自愈。
package dbcache
