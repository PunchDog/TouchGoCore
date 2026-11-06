package taskengine

// Policy 完成通知触发点类型；零值（空串）表示关闭完成通知。
type Policy string

// CompletedNotifyTrigger 读当前完成通知触发点；空串表示关闭。
const (
	CompletedNotifyTrigger Policy = "CompletedNotifyTrigger"
)
