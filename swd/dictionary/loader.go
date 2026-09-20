package dictionary

import (
	"bufio"
	"context"
	_ "embed"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"touchgocore/swd/config"
	"touchgocore/swd/config/mappingdata"
	"touchgocore/swd/core"
	"touchgocore/swd/types/category"
	"touchgocore/util"
)

// Loader 实现core.Loader接口
type Loader struct {
	words           sync.Map
	observers       sync.Map
	notifyBatchSize int
	lastNotifyTime  atomic.Value // time.Time
	notifyInterval  time.Duration
	notifyScheduled atomic.Bool // 是否已有待执行的补发定时器
}

// NewLoader 创建新的加载器实例
func NewLoader() *Loader {
	l := &Loader{
		notifyBatchSize: 100,
		notifyInterval:  time.Millisecond * 100,
	}
	l.lastNotifyTime.Store(util.CurrentTime())
	return l
}

//go:embed default/political.txt
var politicalWords string

//go:embed default/pornography.txt
var pornographyWords string

//go:embed default/violence.txt
var violenceWords string

//go:embed default/gambling.txt
var gamblingWords string

//go:embed default/drugs.txt
var drugsWords string

//go:embed default/profanity.txt
var profanityWords string

//go:embed default/discrimination.txt
var discriminationWords string

//go:embed default/scam.txt
var scamWords string

//go:embed default/all.txt
var allWords string

// LoadDefaultWords 加载默认词库
func (l *Loader) LoadDefaultWords(ctx context.Context) error {
	// 加载所有分类词典
	categories := map[string]struct {
		content string
		cat     category.Category
	}{
		"political.txt":      {content: politicalWords, cat: category.Political},
		"pornography.txt":    {content: pornographyWords, cat: category.Pornography},
		"violence.txt":       {content: violenceWords, cat: category.Violence},
		"gambling.txt":       {content: gamblingWords, cat: category.Gambling},
		"drugs.txt":          {content: drugsWords, cat: category.Drugs},
		"profanity.txt":      {content: profanityWords, cat: category.Profanity},
		"discrimination.txt": {content: discriminationWords, cat: category.Discrimination},
		"scam.txt":           {content: scamWords, cat: category.Scam},
	}

	for filename, data := range categories {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			if err := l.loadFromString(ctx, data.content, data.cat); err != nil {
				return fmt.Errorf("failed to load %s: %w", filename, err)
			}
		}
	}

	// 加载通用词典
	if err := l.loadFromString(ctx, allWords, category.None); err != nil {
		return fmt.Errorf("failed to load all.txt: %w", err)
	}

	l.notifyObserversIfNeeded(true)
	return nil
}

// LoadCustomWords 加载自定义词库
func (l *Loader) LoadCustomWords(ctx context.Context, words map[string]category.Category) error {
	const batchSize = 1000
	count := 0

	for word, cat := range words {
		if count%batchSize == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		}
		if err := l.addWordInternal(word, cat); err != nil {
			return err
		}
		count++
	}

	l.notifyObserversIfNeeded(true)
	return nil
}

// RegisterObserver 注册状态观察者（实现 StateManager 接口）
func (l *Loader) RegisterObserver(observer core.Observer) {
	l.observers.Store(observer, struct{}{})
}

// AddObserver 添加观察者（兼容旧代码）
func (l *Loader) AddObserver(observer core.Observer) {
	l.RegisterObserver(observer)
}

// RemoveObserver 移除状态观察者（实现 StateManager 接口）
func (l *Loader) RemoveObserver(observer core.Observer) {
	l.observers.Delete(observer)
}

// NotifyObservers 通知所有观察者（实现 StateManager 接口）
func (l *Loader) NotifyObservers() {
	l.notifyObserversIfNeeded(true)
}

// notifyObserversIfNeeded 根据条件通知观察者（并发执行）。
// 处于合并窗口内时不是丢弃这次变更，而是延后一个周期补发，避免增量改词后检测器永不重建。
func (l *Loader) notifyObserversIfNeeded(force bool) {
	if !force {
		lastNotify := l.lastNotifyTime.Load().(time.Time)
		if util.CurrentTime().Sub(lastNotify) < l.notifyInterval {
			l.scheduleNotify()
			return
		}
	}

	words := l.GetWords()

	// 收集所有观察者
	var observers []core.Observer
	l.observers.Range(func(key, value interface{}) bool {
		if observer, ok := key.(core.Observer); ok {
			observers = append(observers, observer)
		}
		return true
	})

	// 并发通知所有观察者
	var wg sync.WaitGroup
	for _, observer := range observers {
		wg.Add(1)
		go func(obs core.Observer) {
			defer wg.Done()
			// 捕获 panic，防止单个观察者失败影响其他观察者
			defer func() {
				if r := recover(); r != nil {
					log.Printf("观察者通知失败: %v", r)
				}
			}()
			obs.OnWordsChanged(words)
		}(observer)
	}
	wg.Wait()

	l.lastNotifyTime.Store(util.CurrentTime())
}

// scheduleNotify 安排一次延后补发；同一时刻最多只有一个待执行定时器
func (l *Loader) scheduleNotify() {
	if !l.notifyScheduled.CompareAndSwap(false, true) {
		return
	}
	time.AfterFunc(l.notifyInterval, func() {
		l.notifyScheduled.Store(false)
		l.notifyObserversIfNeeded(false)
	})
}

// AddWord 添加单个敏感词。
// 变更按合并窗口通知观察者（最多延迟 notifyInterval），需要立即生效时调用 NotifyObservers。
func (l *Loader) AddWord(word string, cat category.Category) error {
	if err := l.addWordInternal(word, cat); err != nil {
		return err
	}
	l.notifyObserversIfNeeded(false)
	return nil
}

// addWordInternal 内部添加词方法
func (l *Loader) addWordInternal(word string, cat category.Category) error {
	if word = strings.TrimSpace(word); word == "" {
		return fmt.Errorf("word cannot be empty")
	}

	// 验证分类的有效性
	if !cat.IsValid() {
		return fmt.Errorf("invalid category: %v", cat)
	}

	// 如果词已存在且有效分类，且当前要设置的是 None 分类，则保留原有分类
	if val, exists := l.words.Load(word); exists {
		if existingCat, ok := val.(category.Category); ok && existingCat != category.None && cat == category.None {
			return nil
		}
	}

	l.words.Store(word, cat)
	return nil
}

// AddWords 批量添加敏感词
func (l *Loader) AddWords(words map[string]category.Category) error {
	for word, cat := range words {
		if err := l.addWordInternal(word, cat); err != nil {
			return err
		}
	}
	l.notifyObserversIfNeeded(true)
	return nil
}

// RemoveWord 移除单个敏感词
func (l *Loader) RemoveWord(word string) error {
	l.words.Delete(word)
	l.notifyObserversIfNeeded(false)
	return nil
}

// RemoveWords 批量移除敏感词
func (l *Loader) RemoveWords(words []string) error {
	for _, word := range words {
		l.words.Delete(word)
	}
	l.notifyObserversIfNeeded(true)
	return nil
}

// Clear 清空所有敏感词
func (l *Loader) Clear() error {
	l.words = sync.Map{}
	l.notifyObserversIfNeeded(true)
	return nil
}

// loadFromString 从字符串加载敏感词
func (l *Loader) loadFromString(ctx context.Context, content string, cat category.Category) error {
	reader := strings.NewReader(content)
	return l.loadFromReader(ctx, reader, cat)
}

// loadFromReader 从Reader加载敏感词
func (l *Loader) loadFromReader(ctx context.Context, reader io.Reader, cat category.Category) error {
	scanner := bufio.NewScanner(reader)
	const batchSize = 1000
	count := 0

	for scanner.Scan() {
		if count%batchSize == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		}

		word := strings.TrimSpace(scanner.Text())
		if word == "" || strings.HasPrefix(word, "#") {
			continue
		}
		if err := l.addWordInternal(word, cat); err != nil {
			return err
		}
		count++
	}
	return scanner.Err()
}

// GetWords 获取所有已加载的敏感词
func (l *Loader) GetWords() map[string]category.Category {
	words := make(map[string]category.Category)
	l.words.Range(func(key, value interface{}) bool {
		if k, ok := key.(string); ok {
			if v, ok := value.(category.Category); ok {
				words[k] = v
			}
		}
		return true
	})
	return words
}

// MappingLoader 映射表加载器。
// baseDir 为空时读取随二进制内嵌的映射表；显式指定目录时从磁盘加载，便于运营热替换词表。
type MappingLoader struct {
	mu      sync.RWMutex
	config  *config.MappingConfig
	baseDir string
}

// NewMappingLoader 创建新的映射加载器，baseDir 传空字符串表示使用内嵌数据
func NewMappingLoader(baseDir string) *MappingLoader {
	return &MappingLoader{
		config:  config.NewMappingConfig(),
		baseDir: baseDir,
	}
}

// LoadFromFiles 加载全部映射表，缺少文件或内容非法都会返回错误
func (ml *MappingLoader) LoadFromFiles() error {
	root, err := ml.openRoot()
	if err != nil {
		return err
	}

	tables, err := mappingdata.Parse(root)
	if err != nil {
		return err
	}

	ml.mu.Lock()
	defer ml.mu.Unlock()
	ml.config.UseTables(tables)
	return nil
}

// openRoot 定位映射表所在目录
func (ml *MappingLoader) openRoot() (fs.FS, error) {
	if ml.baseDir == "" {
		return mappingdata.FS(), nil
	}

	dir := ml.baseDir
	// 兼容传入映射表的父目录：<dir>/mappings 存在时优先使用
	sub := filepath.Join(dir, mappingdata.DirName)
	if info, err := os.Stat(sub); err == nil && info.IsDir() {
		dir = sub
	}

	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("映射表目录不可用: %s", dir)
	}
	return os.DirFS(dir), nil
}

// GetConfig 获取加载后的配置
func (ml *MappingLoader) GetConfig() *config.MappingConfig {
	ml.mu.RLock()
	defer ml.mu.RUnlock()
	return ml.config
}

// LoadFromDirectory 从指定目录加载映射
func (ml *MappingLoader) LoadFromDirectory(dir string) error {
	ml.baseDir = dir
	return ml.LoadFromFiles()
}
