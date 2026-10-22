package httpx

import "sync"

// retryableCodes 登记「可以原单重发」的供应商业务错误码。
//
// 空表是刻意的：文档没到位之前，任何业务码一律按不可重试处理。把「余额不足」
// 当限流来重试只是重复挨拒；把出款失败当可重试则有双花风险。确认安全的码
// （限流、网关超时）之后在这里逐字登记，比在调用点散落 if 好收口。
//
// 它从 pay 迁来时刻意不再裸导出：原 map 的裸导出只为编译兼容而留，直接读写
// 不带锁，与解析层的并发读构成 race 面。现在三个入口都带锁，包内判定也一律
// 走 IsRetryableCode——没有一条「看起来能省一次函数调用」的旁路。
var retryableCodes = map[string]struct{}{}

// retryableCodesMu 护住白名单的读写：解析响应（读）可能发生在任何一次查询的
// goroutine 里，登记（写）可能发生在运行期配置热更时——无锁并发读写 map 在 Go
// runtime 是 fatal，不是「偶尔读到旧值」。
var retryableCodesMu sync.RWMutex

// SetRetryableCodes 登记「可以原单重发」的错误码（带锁写入，可重复调用）。
// 空串跳过。登记的每一个码都等于宣布「这个码下原单重发不会双花」，
// 没有供应商文档背书不要登记。
//
// 警示：提现路径白名单严禁纳入 502/504 这类网关状态码——502=请求可能已被
// 供应商受理、只是回执没穿回来，受理状态未知；把「未知」当「失败」原单重发
// 就是双付。同理，任何「不能证明请求未被受理」的码都不得登记。
func SetRetryableCodes(codes ...string) {
	retryableCodesMu.Lock()
	defer retryableCodesMu.Unlock()
	for _, c := range codes {
		if c == "" {
			continue
		}
		retryableCodes[c] = struct{}{}
	}
}

// DeleteRetryableCode 撤销一个已登记的错误码（带锁写入）。
func DeleteRetryableCode(code string) {
	retryableCodesMu.Lock()
	defer retryableCodesMu.Unlock()
	delete(retryableCodes, code)
}

// IsRetryableCode 查询错误码是否在「可原单重发」白名单里（带锁读取）。
// 包内所有判定必须走本函数，不得直读 map。
func IsRetryableCode(code string) bool {
	retryableCodesMu.RLock()
	defer retryableCodesMu.RUnlock()
	_, ok := retryableCodes[code]
	return ok
}

// LenRetryableCodes 返回已登记的白名单条数，仅供启动日志与测试断言用。
func LenRetryableCodes() int {
	retryableCodesMu.RLock()
	defer retryableCodesMu.RUnlock()
	return len(retryableCodes)
}
