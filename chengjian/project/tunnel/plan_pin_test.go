package tunnel

import (
	"strings"
	"testing"
)

// 端口钉死是修「probe 探通 443、deploy 却从 20000+ 池分配」这个必然失败点的关键。
// 这些用例锁定三种行为：钉了就必用、冲突就报错（不静默换口）、没钉就走池。
func TestAllocateListenPortPins(t *testing.T) {
	o := &PlanOpts{PinListenPort: 443, PortMin: 20000, PortMax: 20010}
	got, err := allocateListenPort(o)
	if err != nil {
		t.Fatalf("钉死端口应成功: %v", err)
	}
	if got != 443 {
		t.Fatalf("应使用钉死端口 443, got %d", got)
	}
}

func TestAllocateListenPortRejectsOccupiedPin(t *testing.T) {
	o := &PlanOpts{PinListenPort: 443, UsedPorts: map[int]bool{443: true}}
	_, err := allocateListenPort(o)
	if err == nil {
		t.Fatal("钉死端口被占用时必须报错，不能静默换口")
	}
	if !strings.Contains(err.Error(), "443") {
		t.Fatalf("错误信息应点明端口号: %v", err)
	}
}

func TestAllocateListenPortRejectsUnbindablePin(t *testing.T) {
	o := &PlanOpts{PinListenPort: 443, CanListen: func(int) bool { return false }}
	_, err := allocateListenPort(o)
	if err == nil {
		t.Fatal("钉死端口绑不上时必须报错")
	}
}

func TestAllocateListenPortRejectsOutOfRange(t *testing.T) {
	o := &PlanOpts{PinListenPort: 70000}
	if _, err := allocateListenPort(o); err == nil {
		t.Fatal("超出 65535 的端口必须报错")
	}
}

func TestAllocateListenPortFallsBackToPool(t *testing.T) {
	o := &PlanOpts{PortMin: 20000, PortMax: 20002}
	got, err := allocateListenPort(o)
	if err != nil {
		t.Fatalf("未钉端口应走池分配: %v", err)
	}
	if got < 20000 || got > 20002 {
		t.Fatalf("池分配结果越界: %d", got)
	}
}

// PlanSocks 走完整装配路径时，钉死端口必须真的进到 Plan.ListenPort 与
// PlatformAddr（目标回连地址）——否则 probe 建议的端口传了等于没传。
func TestPlanSocksHonoursPinnedPort(t *testing.T) {
	o := &PlanOpts{
		TaskID: 7, ViaSessionID: 3, CallbackHost: "10.0.0.2",
		PinListenPort: 443, PortMin: 20000, PortMax: 20010,
		DataDir: t.TempDir(),
	}
	p, err := PlanSocks(o)
	if err != nil {
		t.Fatalf("PlanSocks: %v", err)
	}
	if p.ListenPort != 443 {
		t.Fatalf("Plan.ListenPort 应为 443, got %d", p.ListenPort)
	}
	if p.PlatformAddr != "10.0.0.2:443" {
		t.Fatalf("目标回连地址应带 443, got %q", p.PlatformAddr)
	}
}
