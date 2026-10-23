package ton

import "touchgocore/ledger"

// 出款台账与 TRON/BSC/SOL 共用 touchgocore/ledger 的状态机：先记账再广播、
// 同单号不签第二笔、结论不明回 UNKNOWN。本文件只留 TON 这一侧的实例与名字。

// tonLedger 是本包出款动作的台账实例。
//
// 必须是包级实例而不是在通道构造函数里 ledger.New：Start 重复执行时，新通道
// 若换了一本空台账，同一进程内的在途单就再也查不到了。链维度保持 "ton"，
// 已有的持久 journal 键不需要迁移。
var tonLedger = ledger.New("ton")

// ErrOrderAlreadySettled 这一单号已终局，再动就是双付。
var ErrOrderAlreadySettled = ledger.ErrAlreadySettled

// ErrJournalUnavailable 台账写不进去，此时本包放弃广播。
var ErrJournalUnavailable = ledger.ErrJournalUnavailable

// SetJournal 替换 TON 出款台账的存储，返回被换下的那一个；传 nil 即换回进程内实现。
//
// 必须在 TonStart 之前调用：Get/Put 各自加锁但不是一次事务，运行中换存储会让
// 一单的「查台账」和「写台账」落在两个不同的存储上。
func SetJournal(j ledger.Journal) ledger.Journal { return tonLedger.SetJournal(j) }
