package sol

import "touchgocore/ledger"

// 出款台账的实现已在 touchgocore/ledger：TRON / TON / BSC / SOL 这几条会自己动公链的
// 通道共用同一套「先记账再动钱」的状态机，但各持一个实例、键带链维度——只按单号建键
// 的话，一张 SOL 单会撞见同单号 TRON 记录的结论，把「SOL 这单从没发过」读成
// 「已经在链上 settle 了」。本文件只留 SOL 这一侧的实例与名字。

// solLedger 是本包出款动作的台账实例。
var solLedger = ledger.New("sol")

// ErrOrderAlreadySettled 这一单号已终局，再动就是双付（值同 ledger.ErrAlreadySettled，
// 留着是因为调用方按 errors.Is 认它，换个变量名等于悄悄改掉它们的判据）。
var ErrOrderAlreadySettled = ledger.ErrAlreadySettled

// ErrJournalUnavailable 台账写不进去，此时本包放弃广播（口径同 ledger 包）。
var ErrJournalUnavailable = ledger.ErrJournalUnavailable

// SetJournal 替换 SOL 出款台账的存储，返回被换下的那一个；传 nil 即换回进程内实现。
func SetJournal(j ledger.Journal) ledger.Journal { return solLedger.SetJournal(j) }
