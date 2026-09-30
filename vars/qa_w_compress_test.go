package vars

import (
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// ============================================================================
// qa_w 修复回归（槽位 W）：
//   V2 compressFile 在 io.Copy 成功后立即删源文件，gzip Close（CRC 收尾）
//      却在 defer 里且错误被吞 → Close 失败时源已删、.gz 是坏的，数据双失。
// ============================================================================

// qaWFailingCloser 写入正常、Close 必定失败的写入器（模拟磁盘满/IO 错误）
type qaWFailingCloser struct{ f *os.File }

func (w *qaWFailingCloser) Write(p []byte) (int, error) { return w.f.Write(p) }
func (w *qaWFailingCloser) Close() error {
	_ = w.f.Close()
	return errors.New("模拟 Close 失败")
}

// TestQaW_CompressKeepsSourceWhenFinalizeFails 收尾失败时必须保留源文件并清掉坏 .gz。
// 修复前：io.Copy 成功即 os.Remove(源)，Close 失败被 defer 吞掉 → 源没了、归档是坏的。
func TestQaW_CompressKeepsSourceWhenFinalizeFails(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "qa_w_old.log")
	payload := []byte("qa_w 重要日志内容，绝不能双失")
	if err := os.WriteFile(src, payload, 0644); err != nil {
		t.Fatalf("写源文件: %v", err)
	}

	prev := compressionCreate
	compressionCreate = func(name string) (io.WriteCloser, error) {
		f, err := os.Create(name)
		if err != nil {
			return nil, err
		}
		return &qaWFailingCloser{f: f}, nil
	}
	defer func() { compressionCreate = prev }()

	w := &RotatingFileWriter{filePath: dir, fileName: "qa_w"}
	w.compressFile(src)

	if _, err := os.Stat(src); err != nil {
		t.Fatalf("✘ 压缩收尾失败但源文件已被删除（数据双失）: %v", err)
	}
	got, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("✘ 源文件不可读: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatal("✘ 源文件内容受损")
	}
	if _, err := os.Stat(src + ".gz"); err == nil {
		t.Fatal("✘ 收尾失败后留下了损坏的 .gz 归档")
	}
}

// TestQaW_CompressHappyPath 正常路径：源文件删除、.gz 完整可解压且内容一致。
func TestQaW_CompressHappyPath(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "qa_w_ok.log")
	payload := bytesPayload()
	if err := os.WriteFile(src, payload, 0644); err != nil {
		t.Fatalf("写源文件: %v", err)
	}

	w := &RotatingFileWriter{filePath: dir, fileName: "qa_w"}
	w.compressFile(src)

	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("✘ 压缩成功后源文件应删除: err=%v", err)
	}
	f, err := os.Open(src + ".gz")
	if err != nil {
		t.Fatalf("✘ .gz 未生成: %v", err)
	}
	defer f.Close()
	gr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("✘ .gz 头损坏: %v", err)
	}
	data, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("✘ .gz CRC/内容损坏: %v", err)
	}
	if string(data) != string(payload) {
		t.Fatalf("✘ 解压内容不一致: got=%d bytes want=%d bytes", len(data), len(payload))
	}
}

func bytesPayload() []byte {
	var buf []byte
	for i := 0; i < 100; i++ {
		buf = append(buf, []byte("0123456789abcdef 日志行\n")...)
	}
	return buf
}
