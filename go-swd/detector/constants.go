package detector

// 缓存和性能相关常量
const (
	// 缓存大小
	defaultCacheSize = 1000 // 默认预处理器缓存大小
	algoCacheSize   = 10    // 算法选择缓存大小（10个长度区间）

	// 文本长度阈值
	textLengthThreshold = 50 // 长文本阈值
	shortTextLength    = 20 // 短文本阈值

	// 算法选择
	lengthInterval = 5 // 长度区间大小（每5个字符一个区间）
	maxCacheIndex  = 9  // 最大缓存索引（覆盖0-49字符）

	// 批处理
	batchSize       = 1000 // 批量操作大小
	notifyInterval = 100  // 通知间隔（毫秒）

	// 缓存清理
	cacheCleanupThreshold = 500 // 缓存清理阈值（当超过时清理）
	cacheCleanupPercent  = 50   // 缓存清理比例（清理50%）
)
