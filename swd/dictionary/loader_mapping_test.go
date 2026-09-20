package dictionary

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"touchgocore/swd/core"
	"touchgocore/swd/types/category"
)

// recordingObserver 记录每次收到的词库快照
type recordingObserver struct {
	mu       sync.Mutex
	calls    int
	lastWord string
}

func (o *recordingObserver) OnWordsChanged(words map[string]category.Category) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.calls++
	if _, ok := words["合并窗口词"]; ok {
		o.lastWord = "合并窗口词"
	}
}

func (o *recordingObserver) snapshot() (int, string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.calls, o.lastWord
}

var _ core.Observer = (*recordingObserver)(nil)

// TestLoader_ThrottledChangeStillNotifies 合并窗口内的增量改词必须补发通知。
// 历史缺陷：窗口内的 AddWord 直接丢弃通知，词加进去了但检测器永远不重建。
func TestLoader_ThrottledChangeStillNotifies(t *testing.T) {
	loader := NewLoader()
	loader.notifyInterval = 50 * time.Millisecond

	observer := &recordingObserver{}
	loader.AddObserver(observer)

	if err := loader.AddWord("合并窗口词", category.Profanity); err != nil {
		t.Fatalf("AddWord 失败: %v", err)
	}
	if err := loader.AddWord("合并窗口词二", category.Profanity); err != nil {
		t.Fatalf("AddWord 失败: %v", err)
	}

	if calls, _ := observer.snapshot(); calls != 0 {
		t.Fatalf("窗口内不应立即通知，实际已通知 %d 次", calls)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		calls, word := observer.snapshot()
		if calls > 0 {
			if word != "合并窗口词" {
				t.Fatalf("补发的快照未包含新词，收到 calls=%d word=%q", calls, word)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("合并窗口内的词库变更没有补发通知")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestLoader_NotificationsCoalesce 同一窗口内的多次变更合并为一次通知
func TestLoader_NotificationsCoalesce(t *testing.T) {
	loader := NewLoader()
	loader.notifyInterval = 100 * time.Millisecond

	observer := &recordingObserver{}
	loader.AddObserver(observer)

	for i := 0; i < 5; i++ {
		if err := loader.AddWord("批量词", category.Profanity); err != nil {
			t.Fatalf("AddWord 失败: %v", err)
		}
	}
	time.Sleep(400 * time.Millisecond)

	if calls, _ := observer.snapshot(); calls > 1 {
		t.Errorf("窗口内 5 次变更应只通知 1 次，实际 %d 次", calls)
	}
}

// TestMappingLoaderUsesEmbeddedTables 默认加载器必须读到内嵌映射表
func TestMappingLoaderUsesEmbeddedTables(t *testing.T) {
	loader := NewMappingLoader("")
	if err := loader.LoadFromFiles(); err != nil {
		t.Fatalf("加载内嵌映射表失败: %v", err)
	}

	cfg := loader.GetConfig()
	if len(cfg.GetFullWidthToHalf()) == 0 {
		t.Error("全半角映射为空")
	}
	if len(cfg.GetNumberStyle()) == 0 {
		t.Error("数字样式映射为空")
	}
	if len(cfg.GetPinyin()) == 0 {
		t.Error("拼音映射为空")
	}
	if len(cfg.GetHomophone()) == 0 {
		t.Error("同音字映射为空")
	}
	if len(cfg.GetSimilarShape()) == 0 {
		t.Error("形近字映射为空")
	}
}

// TestMappingLoaderDirectoryVariants 传映射目录本身或其父目录都应可用
func TestMappingLoaderDirectoryVariants(t *testing.T) {
	writeTables(t, func(dir string) {
		loader := NewMappingLoader(dir)
		if err := loader.LoadFromFiles(); err != nil {
			t.Fatalf("从 %s 加载映射表失败: %v", dir, err)
		}
		if len(loader.GetConfig().GetHomophone()) == 0 {
			t.Fatalf("从 %s 加载的同音字映射为空", dir)
		}
	})
}

// TestMappingLoaderMissingDirectoryFails 目录不存在时必须报错，而不是静默给出空表
func TestMappingLoaderMissingDirectoryFails(t *testing.T) {
	loader := NewMappingLoader(filepath.Join(t.TempDir(), "not-exists"))
	if err := loader.LoadFromFiles(); err == nil {
		t.Fatal("映射表目录不存在时应当报错")
	}
}

// TestMappingLoaderBrokenTableFails 映射表内容损坏时必须报错
func TestMappingLoaderBrokenTableFails(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fullwidth.txt"), []byte("ab -> A\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	loader := NewMappingLoader(dir)
	if err := loader.LoadFromFiles(); err == nil {
		t.Fatal("映射表内容非法时应当报错")
	}
}

// writeTables 在临时目录分别以“映射目录本身”和“父目录”两种形式落盘映射表
func writeTables(t *testing.T, check func(dir string)) {
	t.Helper()

	files := map[string]string{
		"fullwidth.txt":        "Ｂ -> B\n",
		"numberstyle.txt":      "① -> 1\n",
		"pinyin.txt":           "fa -> 发\n",
		"homophone.txt":        "发 -> 法,罚\n",
		"similar_chinese.txt":  "门 -> 門\n",
		"similar_alphanum.txt": "0 -> O\n",
	}

	write := func(dir string) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	parent := t.TempDir()
	mappings := filepath.Join(parent, "mappings")
	write(mappings)

	check(mappings)
	check(parent)
}
