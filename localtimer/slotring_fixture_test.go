package localtimer

// newTestRing 造与生产同规格的桶环：按精度在 defaultWheelConfigs 里找档序，
// 桶数由同一张精度表推导，避免测试自造一套「只够测试用」的桶数而测不到真实形状。
func newTestRing(config int64) *slotRing {
	for i, c := range defaultWheelConfigs {
		if c == config {
			return newSlotRing(config, slotCountFor(defaultWheelConfigs, i))
		}
	}
	return newSlotRing(config, 64)
}
