package plc

import (
	"sync"
	"testing"
	"time"

	"github.com/lechandonga/soft-plc-runtime-go/internal/simio"
)

// fakeClock 是确定性墙钟：每次调用前进固定步长，测试结果不依赖机器快慢。
type fakeClock struct {
	mu      sync.Mutex
	cur     time.Time
	perCall time.Duration
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.cur
	c.cur = c.cur.Add(c.perCall)
	return t
}

func (c *fakeClock) setPerCall(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.perCall = d
}

// TestScanTimeLimitTriggers 验证：单周期墙钟耗时超上限 -> ScanTimeoutFault，
// 当轮整体回滚、输出落安全值、故障锁存、显式恢复后从最近提交状态继续。
func TestScanTimeLimitTriggers(t *testing.T) {
	src := `
LD I0.0
OUT Q0.0
MOV 5 MW0
END
`
	cfg := DefaultConfig()
	cfg.ScanTimeLimitMS = 5
	cfg.SafeOutputs = map[string]int64{"Q0.0": 0}
	rt, io := newTestRuntime(t, src, cfg)

	clock := &fakeClock{cur: time.Unix(0, 0)}
	rt.now = clock.now

	caseLog(t, "单周期墙钟上限-正常周期不误报", "每拍耗时 1ms，上限 5ms",
		"第1拍正常提交，Q0.0=1、MW0=5 成为最近完整提交状态")
	clock.setPerCall(1 * time.Millisecond)
	io.setBit(0, 0, true)
	if err := rt.Scan(); err != nil {
		t.Fatalf("正常周期不应触发超时: %v", err)
	}
	if !io.outBit(0, 0) {
		t.Fatal("第1拍 Q0.0 应为 1")
	}

	caseLog(t, "单周期墙钟上限-超时触发", "第2拍耗时 10ms（确定性假时钟），上限 5ms",
		"进入 ScanTimeoutFault（不是 WatchdogFault）；当轮回滚；输出整包落安全值；扫描计数不增")
	clock.setPerCall(10 * time.Millisecond)
	err := rt.Scan()
	if err == nil {
		t.Fatal("预期单周期超时")
	}
	f := rt.Fault()
	if f.Kind != FaultScanTimeout {
		t.Fatalf("故障类型必须是 ScanTimeoutFault，与指令预算区分: %s", f.Kind)
	}
	if rt.ScanCount() != 1 {
		t.Fatalf("故障周期不得计入扫描计数: %d", rt.ScanCount())
	}
	// 回滚：超时周期的半截结果一点不留；输出映像为安全值（全 0）。
	if got := rt.SnapshotOutputs()[0]; got != 0 {
		t.Fatalf("超时后输出映像应为安全值 0，got %08b", got)
	}
	if io.outBit(0, 0) {
		t.Fatal("超时后下发到 I/O 的输出应为安全值 0")
	}

	caseLog(t, "单周期墙钟上限-锁存不自动恢复", "I/O 断线再重连（外部条件变化）",
		"扫描仍被锁存，必须显式 Recover")
	io.disconnect()
	io.reconnect()
	if err := rt.Scan(); err == nil || rt.Fault().Kind != FaultScanTimeout {
		t.Fatal("外部条件变化不得让故障悄悄恢复")
	}
	clock.setPerCall(1 * time.Millisecond) // 排除时间因素，恢复后应能正常跑
	if err := rt.Recover(); err != nil {
		t.Fatalf("满足条件后显式恢复应成功: %v", err)
	}
	caseLog(t, "单周期墙钟上限-恢复基线", "Recover 后继续扫描",
		"从最近一次完整提交（MW0=5）继续，超时周期的中间状态不存在")
	if err := rt.Scan(); err != nil {
		t.Fatalf("恢复后扫描失败: %v", err)
	}
	if rt.ScanCount() != 2 {
		t.Fatalf("恢复后扫描计数应推进: %d", rt.ScanCount())
	}
}

// TestScanTimeLimitVsWatchdogDistinct 验证两道单周期保护互相独立、原因可区分。
func TestScanTimeLimitVsWatchdogDistinct(t *testing.T) {
	caseLog(t, "双保护区分-条数超限", "JMP 死循环 + 指令预算10 + 墙钟上限关闭",
		"进入 WatchdogFault 而非 ScanTimeoutFault")
	loopSrc := `
loop:
LD I0.0
JMP loop
END
`
	cfg := DefaultConfig()
	cfg.WatchdogInstructions = 10 // 墙钟上限默认 0 = 关闭
	rt, io := newTestRuntime(t, loopSrc, cfg)
	io.setBit(0, 0, true)
	if err := rt.Scan(); err == nil || rt.Fault().Kind != FaultWatchdog {
		t.Fatalf("预期 WatchdogFault: %v", rt.Fault())
	}

	caseLog(t, "双保护区分-时间超限", "仅3条指令 + 指令预算充足 + 墙钟上限 5ms，假时钟每拍 10ms",
		"进入 ScanTimeoutFault 而非 WatchdogFault（指令没几条但跑得慢也要判）")
	slowSrc := `
LD I0.0
OUT Q0.0
END
`
	cfg2 := DefaultConfig()
	cfg2.ScanTimeLimitMS = 5
	rt2, _ := newTestRuntime(t, slowSrc, cfg2)
	clock := &fakeClock{cur: time.Unix(0, 0), perCall: 10 * time.Millisecond}
	rt2.now = clock.now
	if err := rt2.Scan(); err == nil || rt2.Fault().Kind != FaultScanTimeout {
		t.Fatalf("预期 ScanTimeoutFault: %v", rt2.Fault())
	}
}

// TestScanTimeLimitNoFalsePositiveWithSimIO 验证：真实模拟 I/O 的正常读写开销
// 在合理上限下绝不误报。边界用例不依赖机器快慢。
func TestScanTimeLimitNoFalsePositiveWithSimIO(t *testing.T) {
	src := `
LD I0.0
OUT Q0.0
MOV 1 MW0
END
`
	p, err := Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.ScanTimeLimitMS = 60_000 // 1 分钟上限：正常模拟 I/O 开销远低于此
	mod := simio.NewSimModule(p.InputSize, p.OutputSize)
	rt, err := NewRuntime(src, cfg, mod)
	if err != nil {
		t.Fatal(err)
	}
	caseLog(t, "单周期墙钟上限-正常 I/O 开销不误报", "真实 simio 模块连跑 200 拍，上限 60s",
		"200 拍全部成功、无故障（上限对正常开销有足够裕度，判定确定）")
	for i := 0; i < 200; i++ {
		if err := rt.Scan(); err != nil {
			t.Fatalf("第%d拍误报超时: %v", i+1, err)
		}
	}
	if rt.ScanCount() != 200 {
		t.Fatalf("扫描计数错误: %d", rt.ScanCount())
	}
}

// TestScanTimeLimitDefaultDisabled 验证：缺省（0）关闭墙钟保护，
// 即使单周期“墙钟”再长也不触发，行为与引入该配置前一致。
func TestScanTimeLimitDefaultDisabled(t *testing.T) {
	src := `
LD I0.0
OUT Q0.0
END
`
	rt, _ := newTestRuntime(t, src, DefaultConfig())
	clock := &fakeClock{cur: time.Unix(0, 0), perCall: time.Hour} // 假时钟每拍“走”1小时
	rt.now = clock.now
	caseLog(t, "单周期墙钟上限-缺省关闭", "默认配置 + 假时钟每拍前进1小时",
		"不触发任何超时故障，兼容既有配置与用例")
	for i := 0; i < 5; i++ {
		if err := rt.Scan(); err != nil {
			t.Fatalf("缺省配置不应触发墙钟超时: %v", err)
		}
	}
}

// TestScanTimeLimitHotReloadAtomic 验证墙钟上限走热重载路径原子生效：
// 非法配置整体拒绝，旧配置原样继续；合法新配置立即生效。
func TestScanTimeLimitHotReloadAtomic(t *testing.T) {
	src := `
LD I0.0
OUT Q0.0
END
`
	rt, io := newTestRuntime(t, src, DefaultConfig())
	clock := &fakeClock{cur: time.Unix(0, 0)}
	rt.now = clock.now
	io.setBit(0, 0, true)
	if err := rt.Scan(); err != nil {
		t.Fatal(err)
	}

	caseLog(t, "墙钟上限热重载-非法配置拒绝", "scan_time_limit_ms=-1",
		"Reload Applied=false，旧配置（关闭）继续生效，扫描不受影响")
	badCfg := DefaultConfig()
	badCfg.ScanTimeLimitMS = -1
	if res := rt.Reload(src, badCfg); res.Applied || res.Reason != "ValidationRejected" {
		t.Fatalf("负上限必须拒绝: %+v", res)
	}
	clock.setPerCall(time.Hour) // 旧配置无墙钟上限：不应超时
	if err := rt.Scan(); err != nil {
		t.Fatalf("旧配置应继续生效: %v", err)
	}

	caseLog(t, "墙钟上限热重载-合法配置原子生效", "scan_time_limit_ms=5",
		"Reload Applied=true，下一拍即按新上限判定为 ScanTimeoutFault")
	newCfg := DefaultConfig()
	newCfg.ScanTimeLimitMS = 5
	if res := rt.Reload(src, newCfg); !res.Applied {
		t.Fatalf("合法重载应生效: %+v", res)
	}
	if err := rt.Scan(); err == nil || rt.Fault().Kind != FaultScanTimeout {
		t.Fatalf("新上限应立即生效: %v", rt.Fault())
	}
}
