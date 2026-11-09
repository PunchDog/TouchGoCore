//go:build debug

package localtimer

import "fmt"

// init 只在 `go build -tags debug` / `go test -tags debug` 下参与编译。
// 生产构建没有本文件，assertReleaseArbitration 保持空操作，行为零变化。
func init() {
	assertReleaseArbitration = checkReleaseArbitration
	checkReleaseTable()
}

// checkReleaseTable 校验状态转移表自身的自洽性：命名唯一、guard 齐备、
// 动作与 outcome 合法、仲裁段覆盖全部结论且以兜底行收尾、采样预测正确。
func checkReleaseTable() {
	seen := make(map[string]bool, len(releaseTransitions))
	arbOutcomes := make(map[releaseOutcome]bool)
	var lastArbitration *releaseTransition
	for i := range releaseTransitions {
		tr := &releaseTransitions[i]
		if tr.name == "" {
			panic(fmt.Sprintf("localtimer: releaseTransitions[%d] 缺少 name", i))
		}
		if seen[tr.name] {
			panic(fmt.Sprintf("localtimer: releaseTransitions 名字重复 %q", tr.name))
		}
		seen[tr.name] = true
		if tr.guard == nil {
			panic(fmt.Sprintf("localtimer: %s 缺少 guard", tr.name))
		}
		if tr.action < actionStopOnly || tr.action > actionRejectPut {
			panic(fmt.Sprintf("localtimer: %s 动作越界 %d", tr.name, tr.action))
		}
		if _, ok := releaseActionFor(tr.outcome); !ok {
			panic(fmt.Sprintf("localtimer: %s outcome 未知 %d", tr.name, tr.outcome))
		}
		if tr.phase == phaseArbitration {
			arbOutcomes[tr.outcome] = true
			lastArbitration = tr
		}
	}
	// 仲裁段必须恰好覆盖 4 种「有结论」的 outcome，且末行是无条件兜底。
	if len(arbOutcomes) != 4 || arbOutcomes[releaseNone] {
		panic(fmt.Sprintf("localtimer: 仲裁段 outcome 覆盖不全: %v", arbOutcomes))
	}
	if lastArbitration == nil || !lastArbitration.guard(releaseState{}) {
		panic("localtimer: 仲裁段末行必须是无条件兜底 guard")
	}

	// 代表性状态：表格预测必须与 requestReleaseLocked 的判定顺序逐行一致。
	samples := []struct {
		name string
		s    releaseState
		want releaseOutcome
	}{
		{"no-voucher", releaseState{}, releaseNotPooled},
		{"in-pool", releaseState{fromPool: true, inPool: true}, releaseAlready},
		{"tick-in-flight", releaseState{fromPool: true, inTick: 1}, releaseDeferred},
		{"put-claimed", releaseState{fromPool: true, putClaimed: true}, releaseAlready},
		{"release-now", releaseState{fromPool: true}, releaseNow},
	}
	for _, sp := range samples {
		if got := expectedReleaseOutcome(sp.s); got != sp.want {
			panic(fmt.Sprintf("localtimer: 采样 %s 期望 %v，表格预测 %v", sp.name, sp.want, got))
		}
	}
}

// checkReleaseArbitration 校验运行时的「不可能」组合。
//
// 刻意保守：7 个标志是逐个原子读取的近似快照，inTick 由 beginTick/endTick 在
// t.mu 之外修改，快照与判定之间可能被并发改写。因此这里只断言在无并发歧义时
// 必然成立的约束，不把快照当成精确预测，避免 debug 构建误报。
func checkReleaseArbitration(before releaseState, got releaseOutcome) {
	if _, ok := releaseActionFor(got); !ok {
		panic(fmt.Sprintf("localtimer: requestReleaseLocked 返回未知 outcome %d", got))
	}
	// fromPool 一经设置永不清除：无凭证时仲裁只能给出 releaseNotPooled。
	if !before.fromPool && got != releaseNotPooled {
		panic(fmt.Sprintf("localtimer: 无池凭证却得到 %d, before=%+v", got, before))
	}
	// requestReleaseLocked 恒在 t.mu 内调用，inPool 的变更同样在 t.mu 内，
	// 因此「已在池中」时不可能被并发改写，结论只能是 releaseAlready。
	if before.inPool && got != releaseAlready {
		panic(fmt.Sprintf("localtimer: 已在池中却得到 %d, before=%+v", got, before))
	}
}
