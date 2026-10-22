package pay

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
)

// 本文件钉的是「一单该交给哪条通道」的那张表。
//
// 它是 P5 新写的代码（取代 pay 的驱动注册表 Register/OpenDriver）：键是通道种类，
// 值是已装配的实现。资金安全上它承担一件事——同一单号在一处分派、一个实现执行，
// 不允许出现「两个实现都认这个 kind」的静默覆盖。

// fakeTransfer 记下收到的订单号，用来说明「这一单落到了哪个实现」。
type fakeTransfer struct {
	got []string
	err error
}

func (f *fakeTransfer) Withdraw(_ context.Context, o *PayOrder) (*PayResult, error) {
	if o == nil {
		f.got = append(f.got, "<nil>")
	} else {
		f.got = append(f.got, o.OrderNo)
	}
	if f.err != nil {
		return nil, f.err
	}
	return &PayResult{OrderNo: o.OrderNo, Status: StatusPending}, nil
}

// fakeChannel 是完整统一调用面的假实现：四个方法各自留下调用痕迹。
type fakeChannel struct {
	fakeTransfer
	orderCalls   []string
	accountCalls int
	onchainCalls int
}

func (f *fakeChannel) QueryOrder(_ context.Context, orderNo string) (*PayResult, error) {
	f.orderCalls = append(f.orderCalls, orderNo)
	return &PayResult{OrderNo: orderNo, Status: StatusPending}, nil
}

func (f *fakeChannel) QueryAccount(_ context.Context, q *AccountQuery) (*AccountInfo, error) {
	f.accountCalls++
	return &AccountInfo{Status: AcctActive, Available: 1}, nil
}

func (f *fakeChannel) QueryOnchainAccount(_ context.Context, q *OnchainAccountQuery) (*AccountInfo, error) {
	f.onchainCalls++
	return &AccountInfo{Status: AcctActive, Available: 1}, nil
}

// TestBindRejectsEmptyAndDuplicate 空 kind、空实现、重复登记都必须拒。
//
// 重复登记不许覆盖：覆盖等于让「哪个实现在管这条通道」取决于装配顺序，
// 而两个实现同时活着时，先注册的那个可能已经在出款了。
func TestBindRejectsEmptyAndDuplicate(t *testing.T) {
	if err := Bind("", &fakeTransfer{}); err == nil {
		t.Error("空 Kind 应当被拒")
	}
	if err := Bind(KindTronUSDT, nil); err == nil {
		t.Error("nil 实现应当被拒")
	}
	if err := Bind(KindTronUSDT, &fakeTransfer{}); err != nil {
		t.Fatalf("首次登记失败: %v", err)
	}
	defer Unbind(KindTronUSDT)
	if err := Bind(KindTronUSDT, &fakeTransfer{}); err == nil {
		t.Error("重复登记应当报错，不得静默覆盖")
	} else if !strings.Contains(err.Error(), string(KindTronUSDT)) {
		t.Errorf("重复登记的错误文案应指名种类，实得 %v", err)
	}
}

// TestUnbindAllowsRestart 停掉再起必须能重新登记：Start/Stop 可重复调用，
// Unbind 不释放的话第二次启动会在 Bind 处撞死。
func TestUnbindAllowsRestart(t *testing.T) {
	if err := Bind(KindTON, &fakeTransfer{}); err != nil {
		t.Fatalf("首次登记失败: %v", err)
	}
	if !Unbind(KindTON) {
		t.Fatal("Unbind 应报告确实解开了一个绑定")
	}
	if Unbind(KindTON) {
		t.Fatal("重复 Unbind 不应报告解开了东西")
	}
	if err := Bind(KindTON, &fakeTransfer{}); err != nil {
		t.Fatalf("解开后重新登记失败: %v", err)
	}
	defer Unbind(KindTON)
}

// TestTransferForUnboundListsBound 未装配时要给出明确错误，并列出当前装了哪些——
// 运维看到的是「少装了这条」而不是一句「没有实现」。
func TestTransferForUnboundListsBound(t *testing.T) {
	if _, err := TransferFor(KindBscUSDT); err == nil {
		t.Fatal("未装配的通道应当报错")
	} else if !strings.Contains(err.Error(), string(KindBscUSDT)) {
		t.Errorf("错误文案应指名缺的是哪条通道: %v", err)
	}

	impl := &fakeTransfer{}
	if err := Bind(KindSolUSDT, impl); err != nil {
		t.Fatal(err)
	}
	defer Unbind(KindSolUSDT)
	got, err := TransferFor(KindSolUSDT)
	if err != nil {
		t.Fatalf("已装配却查不到: %v", err)
	}
	if got != impl {
		t.Fatal("取回的不是登记的那个实现")
	}
	if _, err := TransferFor(KindTronUSDT); err == nil ||
		!strings.Contains(err.Error(), string(KindSolUSDT)) {
		t.Errorf("未装配时的错误应列出已装配的通道，实得 %v", err)
	}
}

// TestTransferToDispatchesToBoundImplementation 分派只能落到登记的同一个实现上。
func TestTransferToDispatchesToBoundImplementation(t *testing.T) {
	impl := &fakeTransfer{}
	if err := Bind(KindTronUSDT, impl); err != nil {
		t.Fatal(err)
	}
	defer Unbind(KindTronUSDT)

	res, err := TransferTo(context.Background(), KindTronUSDT, &PayOrder{OrderNo: "O1", Amount: 1, To: &Party{Address: "TAddr"}})
	if err != nil {
		t.Fatalf("分派失败: %v", err)
	}
	if res == nil || res.OrderNo != "O1" {
		t.Fatalf("回执不符: %+v", res)
	}
	if len(impl.got) != 1 || impl.got[0] != "O1" {
		t.Fatalf("实现收到的单号不符: %v", impl.got)
	}

	// 实现侧的错误必须原样透出：分派层不得把「说不清」改写成成功或另一种失败。
	sentinel := errors.New("供应商拒绝")
	buggy := &fakeTransfer{err: sentinel}
	Unbind(KindTronUSDT)
	if err := Bind(KindTronUSDT, buggy); err != nil {
		t.Fatal(err)
	}
	if _, err := TransferTo(context.Background(), KindTronUSDT, &PayOrder{OrderNo: "O2", Amount: 1, To: &Party{Address: "TAddr"}}); !errors.Is(err, sentinel) {
		t.Fatalf("实现错误未被透出: %v", err)
	}

	// 未装配的通道在分派层就要拒，不能返回一个「已受理」。
	if _, err := TransferTo(context.Background(), KindBscUSDT, &PayOrder{OrderNo: "O3", Amount: 1, To: &Party{Address: "0x0"}}); err == nil {
		t.Fatal("未装配的通道不该分派成功")
	}
}

// TestBoundKindsIsSorted 已装配清单必须排序：它进启动日志，
// 顺序随 map 遍历漂移会让同一份配置每次日志都不一样，对账时无法比对。
func TestBoundKindsIsSorted(t *testing.T) {
	if got := BoundKinds(); len(got) != 0 {
		t.Fatalf("初始应无绑定，实得 %v", got)
	}
	for _, k := range []Kind{KindSolUSDT, KindTronUSDT, KindTON} {
		if err := Bind(k, &fakeTransfer{}); err != nil {
			t.Fatal(err)
		}
		defer Unbind(k)
	}
	got := BoundKinds()
	if !sort.StringsAreSorted(got) {
		t.Fatalf("清单未排序: %v", got)
	}
	if len(got) != 3 {
		t.Fatalf("清单项数不符: %v", got)
	}
}

// TestKindsCoverEveryDeclaredChannel 四值枚举与实际分派表必须一致：
// 新增一条链却忘了加进 Kinds()，启动日志与「已装配清单」就会同时漏报。
func TestKindsCoverEveryDeclaredChannel(t *testing.T) {
	want := map[Kind]bool{KindTronUSDT: true, KindTON: true, KindBscUSDT: true, KindSolUSDT: true}
	got := Kinds()
	if len(got) != len(want) {
		t.Fatalf("Kinds()=%v，期望四值", got)
	}
	seen := map[Kind]bool{}
	for _, k := range got {
		if seen[k] {
			t.Fatalf("Kinds() 有重复项: %v", got)
		}
		seen[k] = true
		if !want[k] {
			t.Fatalf("Kinds() 出现未登记的值 %q", k)
		}
	}
}

// TestChannelForKeepsLegacyTransferCompatible 只实现 Withdraw 的旧装配方仍可绑定、
// 仍可分派出款；但查询面必须明确报「未提供」，不能退化成空实现。
func TestChannelForKeepsLegacyTransferCompatible(t *testing.T) {
	legacy := &fakeTransfer{}
	if err := Bind(KindTronUSDT, legacy); err != nil {
		t.Fatalf("旧实现绑定失败: %v", err)
	}
	defer Unbind(KindTronUSDT)

	if _, err := TransferTo(context.Background(), KindTronUSDT, &PayOrder{OrderNo: "O1", Amount: 1, To: &Party{Address: "TAddr"}}); err != nil {
		t.Fatalf("旧实现出款分派失败: %v", err)
	}
	if _, err := ChannelFor(KindTronUSDT); err == nil {
		t.Fatal("只实现 Withdraw 的通道不该被当成完整 Channel")
	} else if !strings.Contains(err.Error(), "统一查询面") {
		t.Fatalf("错误文案应指明缺查询面，实得 %v", err)
	}
}

// TestChannelForDispatchesEveryOperation 四个动作必须落到同一个已登记实现上：
// 统一接口的全部意义就是「按 Kind 找到同一条通道」，不允许查单/查余额各走一条。
func TestChannelForDispatchesEveryOperation(t *testing.T) {
	ch := &fakeChannel{}
	if err := Bind(KindBscUSDT, ch); err != nil {
		t.Fatal(err)
	}
	defer Unbind(KindBscUSDT)

	got, err := ChannelFor(KindBscUSDT)
	if err != nil {
		t.Fatalf("已装配却取不到统一接口: %v", err)
	}
	if got != ch {
		t.Fatal("ChannelFor 返回的不是登记的那个实现")
	}
	res, err := QueryOrderTo(context.Background(), KindBscUSDT, "O1")
	if err != nil || res == nil || res.OrderNo != "O1" {
		t.Fatalf("QueryOrderTo 分派失败: res=%+v err=%v", res, err)
	}
	if _, err := QueryAccountTo(context.Background(), KindBscUSDT, &AccountQuery{}); err != nil {
		t.Fatalf("QueryAccountTo 分派失败: %v", err)
	}
	if _, err := QueryOnchainAccountTo(context.Background(), KindBscUSDT, &OnchainAccountQuery{}); err != nil {
		t.Fatalf("QueryOnchainAccountTo 分派失败: %v", err)
	}
	if len(ch.orderCalls) != 1 || ch.orderCalls[0] != "O1" || ch.accountCalls != 1 || ch.onchainCalls != 1 {
		t.Fatalf("四个动作没有落到同一实现: order=%v account=%d onchain=%d",
			ch.orderCalls, ch.accountCalls, ch.onchainCalls)
	}

	if _, err := QueryOrderTo(context.Background(), KindTronUSDT, "O2"); err == nil {
		t.Fatal("未装配的通道查询不该成功")
	}
}
