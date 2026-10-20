package tron

import "touchgocore/ledger"

// 出款台账的实现已在 touchgocore/ledger（原本包 idempotent.go）：TRON 与 TON 两条会自己
// 动公链的通道共用同一套「先记账再动钱」的状态机，但各持一个实例、键带链维度，
// 免得一张 TON 单撞见同单号 TRON 记录的结论。本文件只留 TRON 这一侧的实例与名字。

// tronLedger 是本包出款动作的台账实例。
var tronLedger = ledger.New("tron")

// ErrOrderAlreadySettled 这一单号已终局，再动就是双付（值同 ledger.ErrAlreadySettled，
// 留着是因为调用方按 errors.Is 认它，换个变量名等于悄悄改掉它们的判据）。
var ErrOrderAlreadySettled = ledger.ErrAlreadySettled

// ErrJournalUnavailable 台账写不进去，此时本包放弃广播（口径同 ledger 包）。
var ErrJournalUnavailable = ledger.ErrJournalUnavailable

// SetJournal 替换 TRON 出款台账的存储，返回被换下的那一个；传 nil 即换回进程内实现。
func SetJournal(j ledger.Journal) ledger.Journal { return tronLedger.SetJournal(j) }
