package dictionary

import (
	"bufio"
	"context"
	_ "embed"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"touchgocore/go-swd/config"
	"touchgocore/go-swd/core"
	"touchgocore/go-swd/types/category"
	"touchgocore/util"
)

// Loader 实现core.Loader接口
type Loader struct {
	words           sync.Map
	observers       sync.Map
	notifyBatchSize int
	lastNotifyTime  atomic.Value // time.Time
	notifyInterval  time.Duration
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

// notifyObserversIfNeeded 根据条件通知观察者（并发执行）
func (l *Loader) notifyObserversIfNeeded(force bool) {
	if !force {
		lastNotify := l.lastNotifyTime.Load().(*time.Time)
		if util.CurrentTime().Sub(*lastNotify) < l.notifyInterval {
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

// AddWord 添加单个敏感词
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

// MappingLoader 映射文件加载器
type MappingLoader struct {
	mu      sync.RWMutex
	config  *config.MappingConfig
	baseDir string
}

// NewMappingLoader 创建新的映射加载器
func NewMappingLoader(baseDir string) *MappingLoader {
	return &MappingLoader{
		config:  config.NewMappingConfig(),
		baseDir: baseDir,
	}
}

// LoadFromFiles 从文件加载所有映射
func (ml *MappingLoader) LoadFromFiles() error {
	ml.mu.Lock()
	defer ml.mu.Unlock()

	// 获取映射文件目录
	if ml.baseDir == "" {
		// 获取当前文件的相对路径
		_, filename, _, _ := runtime.Caller(0)
		ml.baseDir = filepath.Dir(filename)
	}

	mappingsDir := filepath.Join(ml.baseDir, "mappings")

	// 加载全半角映射
	if err := ml.loadFullWidthMap(mappingsDir); err != nil {
		return fmt.Errorf("加载全半角映射失败: %w", err)
	}

	// 加载数字样式映射
	if err := ml.loadNumberStyleMap(mappingsDir); err != nil {
		return fmt.Errorf("加载数字样式映射失败: %w", err)
	}

	// 加载拼音映射
	if err := ml.loadPinyinMap(mappingsDir); err != nil {
		return fmt.Errorf("加载拼音映射失败: %w", err)
	}

	// 加载同音字映射
	if err := ml.loadHomophoneMap(mappingsDir); err != nil {
		return fmt.Errorf("加载同音字映射失败: %w", err)
	}

	// 加载形近字映射
	if err := ml.loadSimilarShapeMap(mappingsDir); err != nil {
		return fmt.Errorf("加载形近字映射失败: %w", err)
	}

	return nil
}

// loadFullWidthMap 加载全半角映射
func (ml *MappingLoader) loadFullWidthMap(dir string) error {
	file := filepath.Join(dir, "fullwidth.txt")
	mapping, err := loadSimpleMapping(file)
	if err != nil {
		return err
	}

	// 转换为 rune->rune 映射
	runesMap := make(map[rune]rune)
	for k, v := range mapping {
		if len(k) == 1 {
			runesMap[rune(k[0])] = rune(v[0])
		}
	}

	ml.config.SetFullWidthToHalf(runesMap)
	return nil
}

// loadNumberStyleMap 加载数字样式映射
func (ml *MappingLoader) loadNumberStyleMap(dir string) error {
	// 合并两个数字样式文件
	file1 := filepath.Join(dir, "numberstyle.txt")
	mapping1, err := loadSimpleMapping(file1)
	if err != nil {
		return err
	}

	runesMap := make(map[rune]rune)
	for k, v := range mapping1 {
		if len(k) == 1 {
			runesMap[rune(k[0])] = rune(v[0])
		}
	}

	ml.config.SetNumberStyle(runesMap)
	return nil
}

// loadPinyinMap 加载拼音映射
func (ml *MappingLoader) loadPinyinMap(dir string) error {
	file := filepath.Join(dir, "pinyin.txt")
	mapping, err := loadSimpleMapping(file)
	if err != nil {
		return err
	}

	// 转换为 string->[]string 映射
	strMap := make(map[string][]string)
	for k, v := range mapping {
		if len(v) > 0 {
			strMap[k] = strings.Split(v, ",")
		}
	}

	ml.config.SetPinyin(strMap)
	return nil
}

// loadHomophoneMap 加载同音字映射
func (ml *MappingLoader) loadHomophoneMap(dir string) error {
	file := filepath.Join(dir, "homophone.txt")
	mapping, err := loadSimpleMapping(file)
	if err != nil {
		return err
	}

	// 转换为 rune->[]rune 映射
	runesMap := make(map[rune][]rune)
	for k, v := range mapping {
		if len(k) == 1 {
			chars := strings.Split(v, ",")
			runes := make([]rune, 0, len(chars))
			for _, c := range chars {
				if len(c) == 1 {
					runes = append(runes, rune(c[0]))
				}
			}
			runesMap[rune(k[0])] = runes
		}
	}

	ml.config.SetHomophone(runesMap)
	return nil
}

// loadSimilarShapeMap 加载形近字映射
func (ml *MappingLoader) loadSimilarShapeMap(dir string) error {
	// 合并两个形近字文件
	runesMap := make(map[rune][]rune)

	// 加载中文形近字
	file1 := filepath.Join(dir, "similar_chinese.txt")
	mapping1, err := loadSimpleMapping(file1)
	if err == nil {
		for k, v := range mapping1 {
			if len(k) == 1 {
				chars := strings.Split(v, ",")
				runes := make([]rune, 0, len(chars))
				for _, c := range chars {
					if len(c) == 1 {
						runes = append(runes, rune(c[0]))
					}
				}
				runesMap[rune(k[0])] = runes
			}
		}
	}

	// 加载字母数字形近字
	file2 := filepath.Join(dir, "similar_alphanum.txt")
	mapping2, err := loadSimpleMapping(file2)
	if err == nil {
		for k, v := range mapping2 {
			if len(k) == 1 {
				chars := strings.Split(v, ",")
				runes := make([]rune, 0, len(chars))
				for _, c := range chars {
					if len(c) == 1 {
						runes = append(runes, rune(c[0]))
					}
				}
				// 避免覆盖中文映射
				if _, exists := runesMap[rune(k[0])]; !exists {
					runesMap[rune(k[0])] = runes
				}
			}
		}
	}

	ml.config.SetSimilarShape(runesMap)
	return nil
}

// loadSimpleMapping 加载简单的键值对映射文件
func loadSimpleMapping(file string) (map[string]string, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	mapping := make(map[string]string)
	scanner := bufio.NewScanner(f)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// 跳过空行和注释
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// 解析键值对
		parts := strings.SplitN(line, "->", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		mapping[key] = value
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return mapping, nil
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

// LoadFromReader 从 Reader 加载映射（用于测试）
func LoadFromReader(reader io.Reader) (*config.MappingConfig, error) {
	cfg := config.NewMappingConfig()

	// 这里简化实现，实际可以从 reader 读取各个映射
	// 并设置到 cfg 中

	return cfg, nil
}
