package taskengine

import "errors"

// 引擎对外暴露的错误哨兵。宿主按 errors.Is 映射到自己的错误码/HTTP 响应，
// 本包不依赖任何传输层。
var (
	// ErrDefMissing 任务定义不存在。
	ErrDefMissing = errors.New("taskengine: task definition missing")
	// ErrDisabled 任务未启用。
	ErrDisabled = errors.New("taskengine: task disabled")
	// ErrNotEligible match 条件未通过、窗口外或前置未完成。
	ErrNotEligible = errors.New("taskengine: not eligible")
	// ErrNotComplete 进度未达到 TargetCount。
	ErrNotComplete = errors.New("taskengine: target not met")
	// ErrAlreadyClaimed 本周期已结算。
	ErrAlreadyClaimed = errors.New("taskengine: already claimed")
	// ErrStoreNotConfigured Store/DB 未装配。
	ErrStoreNotConfigured = errors.New("taskengine: store not configured")
	// 创建node节点失败
	ErrCreateEngineFail = errors.New("task engine:create engine fail")
)
