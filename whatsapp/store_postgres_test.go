package whatsapp

// 本文件是删除闸门（waPinnedContainer）的活闸。
//
// 闸的对象是库侧那两条「服务端一句话就抹掉本机配对凭据」的路径：它们全部收敛到
// Device.Delete 这一个入口（查自 WM/store/store.go:286-298），而 Device.Delete 唯一
// 的写动作就是 device.Container.DeleteDevice——所以拦住这个字段就等于拦住那两条路。
// 测试直接调 dev.Delete，就是库在 connectionevents.go:44 / :129 里调的那一句。

import (
	"context"
	"errors"
	"testing"

	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
)

// fakeDeviceContainer 是 store.DeviceContainer 的记账假件（两方法，逐字照库接口）。
type fakeDeviceContainer struct {
	puts    int
	deletes int
	putErr  error
}

func (f *fakeDeviceContainer) PutDevice(context.Context, *store.Device) error {
	f.puts++
	return f.putErr
}

func (f *fakeDeviceContainer) DeleteDevice(context.Context, *store.Device) error {
	f.deletes++
	return nil
}

// newPinnedTestDevice 造一个「库里查得到的已配对设备」并套闸门。
func newPinnedTestDevice() (*store.Device, *fakeDeviceContainer) {
	inner := &fakeDeviceContainer{}
	jid := types.NewJID("8613800000000", types.DefaultUserServer)
	dev := &store.Device{ID: &jid, Container: inner}
	return pinWADevice(dev), inner
}

// TestPinBlocksLibraryAutoDelete 库侧删行被拦：底层一次都没被调用，
// 但库自己的内存收尾照走（ID 置 nil、Deleted 置位），死会话不会再往库里写。
func TestPinBlocksLibraryAutoDelete(t *testing.T) {
	dev, inner := newPinnedTestDevice()

	if err := dev.Delete(context.Background()); err != nil {
		t.Fatalf("被拦下的删行应当返回 nil（让库走完内存收尾），实得 %v", err)
	}
	if inner.deletes != 0 {
		t.Fatalf("闸门漏放：底层 DeleteDevice 被调了 %d 次，期望 0", inner.deletes)
	}
	if dev.ID != nil {
		t.Fatalf("库侧的 ID 收尾没走到（期望 nil，实得 %s），设备子存储仍是可写态", dev.ID.String())
	}
	if !dev.Deleted {
		t.Fatal("dev.Deleted 未置位，库会继续用这套内存态写数据")
	}
}

// TestPinForwardsPutDevice 落库这条路必须原样通：配对成功靠 Save→PutDevice 写行，
// 拦了它等于扫码配对永远不生效。
func TestPinForwardsPutDevice(t *testing.T) {
	dev, inner := newPinnedTestDevice()

	if err := dev.Save(context.Background()); err != nil {
		t.Fatalf("Save 应当透传成功，实得 %v", err)
	}
	if inner.puts != 1 {
		t.Fatalf("PutDevice 调用数=%d，期望 1（闸门只拦删、不拦落库）", inner.puts)
	}

	inner.putErr = errors.New("boom")
	if err := dev.Save(context.Background()); err == nil {
		t.Fatal("底层 PutDevice 的错误必须原样上抛，不能被闸门吞掉")
	}
}

// TestPinIdempotentAndNilSafe 重复套闸门不叠加（叠加会让日志与内层指针绕圈），
// 且 nil 设备/nil Container 就地返回而不是 panic——GetDevice 的「查无」形态是 (nil, nil)。
func TestPinIdempotentAndNilSafe(t *testing.T) {
	dev, inner := newPinnedTestDevice()
	again := pinWADevice(dev)
	if again != dev {
		t.Fatal("pinWADevice 必须原地返回同一对象（管理器持有的是这个指针）")
	}
	if _, ok := dev.Container.(waPinnedContainer); !ok {
		t.Fatalf("Container 类型=%T，期望 waPinnedContainer", dev.Container)
	}
	if pinWADevice(nil) != nil {
		t.Fatal("nil 设备必须原样返回 nil")
	}
	bare := &store.Device{}
	if got := pinWADevice(bare); got != bare || got.Container != nil {
		t.Fatal("Container 为空的设备不该被硬套闸门")
	}

	// 内层指针必须还是那个真容器：显式吊销（WAStore.DeleteDevice）不经闸门，
	// 这里从包装里取回内层，确证被拦的不是唯一出口。
	wrapped, ok := dev.Container.(waPinnedContainer)
	if !ok || wrapped.inner != store.DeviceContainer(inner) {
		t.Fatal("闸门丢了内层容器引用，显式删行将无路可走")
	}
}
