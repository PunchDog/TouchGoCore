package pay

// Secret 是「只能进签名流程、不能进日志与序列化」的字符串。
//
// 它出现在 Party.PrivateKey 上：按单出款需要把热钱包私钥从上游带到通道包，
// 而 PayOrder 会经过日志、回调与错误包装。类型本身把这三条出口都堵住：
//   - fmt 的 %v/%+v/%s 走 String()，%#v 走 GoString()，一律打码；
//   - JSON 走 MarshalJSON，输出 null（Party 上的 json:"-" 是第二道）；
//   - 真要取值只能显式 Reveal()，调用点因此可被 grep 出来审计。
//
// 它不是加密容器：只负责「不被顺手打印」。真正的边界仍是「私钥只在内存里、
// 只在签名那一刻出现」。
type Secret string

// String 让 fmt 的常规动词只看到掩码。
func (s Secret) String() string { return "***" }

// GoString 让 fmt 的 %#v 也只看到掩码。
func (s Secret) GoString() string { return "***" }

// MarshalJSON 保证直接序列化 Secret 也不会泄露明文。
func (s Secret) MarshalJSON() ([]byte, error) { return []byte("null"), nil }

// Reveal 取出明文。调用点必须限定在签名/派生地址这类必须用到私钥的地方。
func (s Secret) Reveal() string { return string(s) }
