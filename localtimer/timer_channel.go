package localtimer

// shardCapFor 单片容量：总容量按片数均分（S68），非法片数按 1 片算，且不低于 1。
func shardCapFor(n int) int64 {
	if n < 1 {
		n = 1
	}
	per := MaxTimerChannelNum / int64(n)
	if per < 1 {
		per = 1
	}
	return per
}

// ensureTimerChannel 确保调度通道已创建（可被 Stop 后重建）。
//
// 分片数在建通道这一刻定死（S68）：每片容量为 MaxTimerChannelNum/分片数，
// 因此「整套通道的总容量」与既往单通道时一致——放开消费者不该顺带把
// 允许的积压量放大 N 倍，那只会把一个慢消费者的堆积摊成 N 份。
func ensureTimerChannel() {
	timerChannelMu.Lock()
	defer timerChannelMu.Unlock()
	if timerChannelReady && len(timerChannels) > 0 {
		return
	}
	n := effectiveShards()
	per := shardCapFor(n)
	channels := make([]chan timerTask, n)
	for i := range channels {
		channels[i] = make(chan timerTask, per)
	}
	timerChannels = channels
	timerChannelReady = true
}

// resetTimerChannel 释放调度通道。
// 刻意不 close：未被管理器纳管（或停止后仍在运行）的时间轮可能仍在发送，
// close 会直接触发 "send on closed channel" 的致命 panic。
func resetTimerChannel() {
	timerChannelMu.Lock()
	defer timerChannelMu.Unlock()
	timerChannels = nil
	timerChannelReady = false
}

// currentTimerChannels 返回当前调度通道数组快照（无通道时为 nil）。
// 快照整体取自一次加锁，消费端与统计端不会读到「长度与元素不同源」的半成品。
func currentTimerChannels() []chan timerTask {
	timerChannelMu.Lock()
	defer timerChannelMu.Unlock()
	return timerChannels
}

// currentTimerChannel 返回 0 号分片通道（无通道时为 nil）。
//
// 未开启分片（默认）时它就是唯一的调度通道；开了分片时只代表第 0 片。
// 保留这个入口是给「直投/直排单条通道」的测试与统计用的，生产派发一律走
// shardOf，不要拿本函数当「那条通道」。
func currentTimerChannel() chan timerTask {
	if channels := currentTimerChannels(); len(channels) > 0 {
		return channels[0]
	}
	return nil
}

// scheduleChannelAt 取第 shard 号分片通道。数组尚未建立或下标越界时返回 nil。
func scheduleChannelAt(shard int) chan timerTask {
	channels := currentTimerChannels()
	if shard < 0 || shard >= len(channels) {
		return nil
	}
	return channels[shard]
}

// shardOf 在通道数组快照里按 uid 取分片通道。
//
// 派发路径拿快照而不是逐节点查全局：一次 tick 要派发几十个节点，逐节点加锁读
// 通道数组等于把分片决策成本重新贴回轮协程上（实测 256 节点 1.7µs 对 10.2µs）。
//
// 空快照返回 nil 是可接受的入参：commitDispatch 的投递是非阻塞 select，nil 分支
// 永不就绪，于是走 default 判「通道满」，节点留在原轮等下一拍重试——与未 Run 时
// 的既往行为一致。
func shardOf(channels []chan timerTask, uid int64) chan timerTask {
	if len(channels) == 0 {
		return nil
	}
	if uid < 0 {
		uid = -uid
	}
	return channels[uid%int64(len(channels))]
}
