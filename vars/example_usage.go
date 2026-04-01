package vars

import (
	"log/slog"
	"strings"
	"time"

	"go.uber.org/zap"
)

// ExampleUsage 展示优化后日志系统的使用方法
func ExampleUsage() {
	// 1. 使用默认配置初始化
	// InitializeOptimizedWithDefaults()

	// 2. 使用自定义配置初始化
	cfg := DefaultConfig()
	cfg.LogName = "myapp"
	cfg.LogLevel = LogLevelInfo
	cfg.Async = true
	cfg.Fields = map[string]string{
		"service": "my-service",
		"version": "1.0.0",
		"env":     "production",
	}

	// InitializeOptimized(cfg)

	// 3. 基本日志记录
	Debug("This is a debug message")
	Info("Service started successfully")
	Warning("Configuration using default values")
	Error("Failed to connect to database")

	// 4. 使用优化后的日志函数
	DebugOpt("Optimized debug message")
	InfoOpt("Optimized info message")
	WarnOpt("Optimized warn message")
	ErrorOpt("Optimized error message")

	// 5. 结构化日志（带字段）
	DebugWithFields("User action",
		slog.String("action", "login"),
		slog.String("user_id", "12345"),
		slog.Duration("duration", 150*time.Millisecond),
	)

	InfoWithFields("Request completed",
		slog.String("method", "GET"),
		slog.String("path", "/api/users"),
		slog.Int("status_code", 200),
	)

	// 6. 格式化消息
	Info("User %s logged in from %s", "john.doe", "192.168.1.1")

	// 7. 错误处理示例
	err := testFunction()
	if err != nil {
		ErrorWithFields("Operation failed",
			slog.String("operation", "testFunction"),
			slog.Any("error", err),
		)
	}

	// 8. 使用LoggerManager进行高级操作
	manager := GetOptimizedLogger()

	// 动态设置日志级别
	manager.SetLevel(LogLevelDebug)

	// 检查是否启用
	if manager.IsEnabled() {
		Info("Logging is enabled")
	}

	// 获取统计信息
	stats := manager.GetStats()
	if stats.Enabled {
		Info("Logger is active")
	}

	// 关闭日志器（优雅关闭）
	// ShutdownOptimized()
}

// testFunction 测试函数
func testFunction() error {
	return nil
}

// ExampleAdvancedUsage 高级用法示例
func ExampleAdvancedUsage() {
	// 1. 创建带有轮转功能的日志器
	cfg := DefaultConfig()
	cfg.LogName = "advanced"
	cfg.MaxSize = 50  // 50MB
	cfg.MaxAge = 7    // 7天
	cfg.MaxBackups = 5 // 保留5个备份
	cfg.Compress = true

	// InitializeOptimized(cfg)

	// 2. 异步日志性能测试
	start := time.Now()
	for i := 0; i < 10000; i++ {
		InfoOpt("Async log test message %d", i)
	}
	duration := time.Since(start)
	Info("Logged 10000 messages in %v", duration)

	// 3. 不同级别的日志过滤
	cfg.LogLevel = LogLevelWarn
	// 重新初始化会应用新的日志级别
	// InitializeOptimized(cfg)

	Debug("This debug message won't appear")
	Info("This info message won't appear")
	Warning("This warning will appear")
	Error("This error will appear")

	// 4. 日志分组
	logger := GetOptimizedLogger().GetLogger()
	groupLogger := logger.WithGroup("http")

	groupLogger.Info("Request started",
		slog.String("method", "POST"),
		slog.String("url", "/api/create"),
	)

	// 5. 性能关键路径使用Zap直接访问
	zapLogger := GetOptimizedLogger().GetZapLogger()
	zapLogger.Info("Direct zap access for high performance",
		zap.String("component", "performance-critical"),
		zap.Int("iterations", 1000000),
	)
}

// ExampleContextLogging 带上下文的日志示例
func ExampleContextLogging() {
	// ctx := context.Background()
	// ctx = context.WithValue(ctx, "request_id", "req-123")

	_ = GetOptimizedLogger()

	// manager.LogWithContext(ctx, slog.LevelInfo,
	// 	"Processing request",
	// 	slog.String("path", "/api/process"),
	// )
}

// ExampleErrorHandling 错误处理示例
func ExampleErrorHandling() {
	err := func() error {
		return nil
	}()

	if err != nil {
		// 使用结构化字段记录错误详情
		ErrorWithFields("Database connection failed",
			slog.String("host", "localhost"),
			slog.Int("port", 5432),
			slog.Duration("timeout", 5*time.Second),
			slog.Any("error", err),
		)
	}

	// 记录堆栈信息
	err = testFunction()
	if err != nil {
		ErrorWithFields("Critical error occurred",
			slog.String("error", err.Error()),
			slog.Bool("recovered", false),
		)
	}
}

// ExampleLogRotation 日志轮转示例
func ExampleLogRotation() {
	cfg := DefaultConfig()
	cfg.LogName = "rotation_test"
	cfg.MaxSize = 1    // 1MB - 小值用于测试
	cfg.MaxAge = 1    // 1天
	cfg.MaxBackups = 3
	cfg.Compress = true

	// InitializeOptimized(cfg)

	// 生成大量日志以触发轮转
	for i := 0; i < 10000; i++ {
		Info("Log rotation test message %d - %s", i, strings.Repeat("x", 100))
	}

	Info("Log rotation test completed")
}

// ExampleAsyncLogging 异步日志示例
func ExampleAsyncLogging() {
	cfg := DefaultConfig()
	cfg.LogName = "async_test"
	cfg.Async = true
	cfg.AsyncBufferSize = 50000

	// InitializeOptimized(cfg)

	// 高吞吐量日志
	for i := 0; i < 50000; i++ {
		InfoOpt("High throughput log message %d", i)
	}

	// 等待异步写入器刷新
	// time.Sleep(1 * time.Second)

	Info("Async logging test completed")
}

// ExampleFieldUsage 字段使用示例
func ExampleFieldUsage() {
	// 基本字段
	InfoWithFields("Basic fields",
		slog.String("username", "john"),
		slog.Int("age", 30),
		slog.Bool("active", true),
		slog.Float64("score", 95.5),
	)

	// 时间和持续时间
	InfoWithFields("Time fields",
		slog.Time("timestamp", time.Now()),
		slog.Duration("elapsed", 250*time.Millisecond),
	)

	// 数组和对象
	InfoWithFields("Complex fields",
		slog.Any("tags", []string{"golang", "logging", "performance"}),
		slog.Any("metadata", map[string]interface{}{
			"version": "2.0.0",
			"build":   "dev-12345",
		}),
	)

	// 嵌套分组
	logger := GetOptimizedLogger().GetLogger()
	apiLogger := logger.WithGroup("api").WithGroup("v2")

	apiLogger.Info("API endpoint called",
		slog.String("endpoint", "/users"),
		slog.String("method", "GET"),
	)
}

// ExampleProductionUsage 生产环境使用示例
func ExampleProductionUsage() {
	// 生产环境配置
	cfg := DefaultConfig()
	cfg.LogName = "production"
	cfg.LogLevel = LogLevelInfo      // 生产环境使用Info级别
	cfg.MaxSize = 100               // 100MB
	cfg.MaxAge = 30                 // 保留30天
	cfg.MaxBackups = 20             // 保留20个备份
	cfg.Compress = true             // 压缩旧日志
	cfg.Stdout = false              // 生产环境通常不输出到stdout
	cfg.Async = true                // 使用异步日志提升性能
	cfg.AsyncBufferSize = 100000    // 大缓冲区
	cfg.CallerSkip = 2              // 跳过调用栈
	cfg.Fields = map[string]string{
		"environment": "production",
		"cluster":     "us-east-1",
		"service":     "api-server",
		"version":     "2.1.0",
	}

	// InitializeOptimized(cfg)

	// 生产环境日志示例
	InfoWithFields("Application started",
		slog.Time("startup_time", time.Now()),
		slog.String("hostname", getHostname()),
		slog.Int("pid", getCurrentPID()),
	)

	InfoWithFields("Health check passed",
		slog.String("check", "database"),
		slog.Int("latency_ms", 5),
	)
}

// 辅助函数
func getHostname() string {
	return "server-001"
}

func getCurrentPID() int {
	return 12345
}
