package plc

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock 是可注入的确定性虚拟墙钟：每次 now() 调用推进固定步长，
// 超时判定完全由步长与预算决定，与机器快慢无关（反复跑、-race 下结论一致）。
type fakeClock struct {
	mu   sync.Mutex
	t    time.Time
	step time.Duration
}

func newFakeClock(step time.Duration) *fakeClock {
	return &fakeClock{t: time.Unix(0, 0), step: step}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(c.step)
	return c.t
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// timedIO 给 fakeIO 的每次整包读写叠加确定的虚拟耗时，用来覆盖
// “采样/提交开销计入墙钟”这条边界。
type timedIO struct {
	*fakeIO
	clk                 *fakeClock
	readCost, writeCost time.Duration
}

func (m *timedIO) Read(size int) ([]byte, error) {
	m.clk.add(m.readCost)
	return m.fakeIO.Read(size)
}

func (m *timedIO) Write(out []byte) error {
	m.clk.add(m.writeCost)
	return m.fakeIO.Write(out)
}

func newDeadlineRuntime(t *testing.T, src string, cfg Config, clk *fakeClock, readCost, writeCost time.Duration) (*Runtime, *timedIO) {
	t.Helper()
	p, err := Parse(src)
	if err != nil {
		t.Fatalf("测试程序非法: %v", err)
	}
	fio := newFakeIO(p.InputSize, p.OutputSize)
	io := &timedIO{fakeIO: fio, clk: clk, readCost: readCost, writeCost: writeCost}
	rt, err := NewRuntime(src, cfg, io)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	rt.now = clk.now
	return rt, io
}

// deadlineProgram 先把 I0.1 透传到 M0.0/Q0.0（正常周期可完整提交），
// M0.1 是“半截写入探针”（故障周期由 I0.2 置 1，此前已提交值为 0），
// 再在 I0.0 为真时进入 2 指令死循环，供墙钟/条数两道保护分别演练。
const deadlineProgram = `
LD I0.1
OUT M0.0
LD M0.0
OUT Q0.0
LD I0.2
OUT M0.1
loop:
LD I0.0
JMP loop
END
`

// TestWallClockDeadlineTriggers 验证墙钟上限在指令远未超条数时独立触发，
// 故障原因明确为“时间到点”，当轮回滚、输出安全值、锁存且只能显式恢复。
func TestWallClockDeadlineTriggers(t *testing.T) {
	cfg := DefaultConfig()
	cfg.WatchdogInstructions = 1_000_000 // 条数预算极宽裕，排除条数因素
	cfg.WatchdogTimeMS = 50
	cfg.SafeOutputs = map[string]int64{"Q0.0": 0}

	clk := newFakeClock(2 * time.Millisecond) // 每条指令边界固定推进 2ms；正常周期约 22ms
	rt, io := newDeadlineRuntime(t, deadlineProgram, cfg, clk, time.Millisecond, time.Millisecond)

	// 第 1 周期：I0.0=0 不循环，I0.1=1 完整提交 M0.0=Q0.0=1。
	io.setBit(0, 1, true)
	if err := rt.Scan(); err != nil {
		t.Fatalf("正常周期不应超时: %v", err)
	}
	if !io.outBit(0, 0) {
		t.Fatal("正常周期 Q0.0 应为 1")
	}

	// 第 2 周期：I0.1=0，I0.2=1（M0.1 会被写成 1 后才进循环），I0.0=1 进入死循环；
	// 墙钟在确定的指令边界到点，与指令条数无关。
	io.setBit(0, 1, false)
	io.setBit(0, 2, true)
	io.setBit(0, 0, true)
	caseLog(t, "墙钟保护-慢循环时间到点", "条数预算100万、墙钟50ms、每指令虚拟耗时2ms",
		"WatchdogFault 且明细为墙钟到点；M0.0 半截写入回滚；输出整包安全值0；锁存")
	err := rt.Scan()
	if err == nil {
		t.Fatal("预期墙钟超时")
	}
	f := rt.Fault()
	if f.Kind != FaultWatchdog {
		t.Fatalf("故障类型错误: %s", f.Kind)
	}
	if !strings.Contains(f.Detail, "墙钟耗时") {
		t.Fatalf("故障原因必须标明时间到点，got: %s", f.Detail)
	}
	var flt *Fault
	if !errors.As(err, &flt) {
		t.Fatalf("Scan 应返回锁存的 *Fault，got %T", err)
	}
	if !strings.Contains(flt.Detail, "超过上限 50ms") {
		t.Fatalf("故障明细应含墙钟上限数据，got: %s", flt.Detail)
	}
	// 当轮整体回滚：半截探针 M0.1 必须回到周期前的 0；M0.0 保持最近提交值 1。
	mem := rt.SnapshotMemory()
	if mem[0]&0x02 != 0 {
		t.Fatalf("超时周期的中间写入 M0.1 必须回滚，Mem=%08b", mem[0])
	}
	if mem[0]&0x01 == 0 {
		t.Fatalf("回滚基线必须是最近一次完整提交（M0.0=1），Mem=%08b", mem[0])
	}
	// 输出整包安全值并已下发。
	if rt.SnapshotOutputs()[0] != 0 || io.outBit(0, 0) {
		t.Fatal("超时后输出必须整包落安全值 0")
	}
	// 锁存：外部输入变化不自动恢复，扫描不推进。
	io.setBit(0, 0, false)
	before := rt.ScanCount()
	if err := rt.Scan(); err == nil || rt.ScanCount() != before {
		t.Fatal("墙钟故障必须锁存，不得因外部变化自行恢复")
	}
	if err := rt.Recover(); err != nil {
		t.Fatalf("满足条件后显式恢复应成功: %v", err)
	}
	// 恢复后从最近一次完整提交继续：I0.1=1 时输出重新建立，且不残留半成品。
	io.setBit(0, 1, true)
	if err := rt.Scan(); err != nil {
		t.Fatalf("恢复后扫描失败: %v", err)
	}
	if !io.outBit(0, 0) {
		t.Fatal("恢复后应从最近提交状态继续正常扫描")
	}
}

// TestWallClockDeadlineNoFalsePositive 验证正常 I/O 与执行开销不会误报：
// 虚拟时钟把每拍开销钉死在远低于上限的水平；另用真实时钟连跑多拍兜底。
func TestWallClockDeadlineNoFalsePositive(t *testing.T) {
	cfg := DefaultConfig()
	cfg.WatchdogTimeMS = 50
	clk := newFakeClock(10 * time.Microsecond) // 每指令边界 10µs
	rt, io := newDeadlineRuntime(t, deadlineProgram, cfg, clk, 20*time.Microsecond, 20*time.Microsecond)
	io.setBit(0, 1, true)
	caseLog(t, "墙钟保护-正常开销不误报", "墙钟50ms，采样/提交各20µs、每指令10µs（虚拟）",
		"30 个正常周期全部成功，无故障、计数精确为 30")
	for i := 0; i < 30; i++ {
		if err := rt.Scan(); err != nil {
			t.Fatalf("第%d周期被墙钟误报: %v", i+1, err)
		}
	}
	if rt.ScanCount() != 30 {
		t.Fatalf("成功周期数错误: %d", rt.ScanCount())
	}

	// 真实时钟兜底：模拟 I/O 的实际读写开销必须远低于宽松上限。
	cfg2 := DefaultConfig()
	cfg2.WatchdogTimeMS = 2000
	p, _ := Parse(deadlineProgram)
	rt2, err := NewRuntime(deadlineProgram, cfg2, newFakeIO(p.InputSize, p.OutputSize))
	if err != nil {
		t.Fatal(err)
	}
	fio2 := rt2.io.(*fakeIO)
	fio2.setBit(0, 1, true)
	for i := 0; i < 50; i++ {
		if err := rt2.Scan(); err != nil {
			t.Fatalf("真实时钟下第%d周期误报: %v", i+1, err)
		}
	}
}

// TestWallClockCoversSampleAndCommit 验证计时范围覆盖采样与输出提交：
// 执行体本身极短，仅采样或提交超预算也必须判墙钟超时。
func TestWallClockCoversSampleAndCommit(t *testing.T) {
	src := `
LD I0.0
OUT Q0.0
END
`
	t.Run("采样阶段耗时超预算", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.WatchdogTimeMS = 50
		clk := newFakeClock(time.Millisecond)
		rt, io := newDeadlineRuntime(t, src, cfg, clk, 60*time.Millisecond, time.Millisecond)
		io.setBit(0, 0, true)
		caseLog(t, "墙钟保护-采样慢也算超时", "墙钟50ms，Read 固定耗60ms（虚拟）",
			"采样后判定 WatchdogFault（墙钟），输出安全值")
		err := rt.Scan()
		if err == nil || rt.Fault().Kind != FaultWatchdog {
			t.Fatalf("预期墙钟故障: %v", err)
		}
		if !strings.Contains(rt.Fault().Detail, "墙钟耗时") {
			t.Fatalf("必须标明墙钟到点: %s", rt.Fault().Detail)
		}
	})
	t.Run("提交阶段耗时超预算", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.WatchdogTimeMS = 50
		clk := newFakeClock(time.Millisecond)
		rt, io := newDeadlineRuntime(t, src, cfg, clk, time.Millisecond, 60*time.Millisecond)
		io.setBit(0, 0, true)
		caseLog(t, "墙钟保护-提交慢也算超时", "墙钟50ms，Write 固定耗60ms（虚拟）",
			"提交完成后判定 WatchdogFault（墙钟），当轮回滚落安全值")
		err := rt.Scan()
		if err == nil || rt.Fault().Kind != FaultWatchdog {
			t.Fatalf("预期墙钟故障: %v", err)
		}
		if io.outBit(0, 0) {
			t.Fatal("提交超时后安全值必须覆盖当轮输出")
		}
	})
}

// TestDeadlineVsInstructionBudget 验证两道保护互相独立、原因可区分、谁先触发算谁。
func TestDeadlineVsInstructionBudget(t *testing.T) {
	src := `
loop:
LD I0.0
JMP loop
END
`
	p, _ := Parse(src)
	d := newDataTable(p)
	d.In[0] = 1 // I0.0 恒真，确定进入死循环

	t.Run("仅条数超-原因是条数", func(t *testing.T) {
		caseLog(t, "双保护-仅条数触发", "墙钟关闭，条数预算3",
			"Execute 返回 *WatchdogError（条数超了）")
		err := Execute(p, d, 10, ExecConfig{MaxInstructions: 3})
		var wd *WatchdogError
		if !errors.As(err, &wd) {
			t.Fatalf("预期 *WatchdogError，got %T %v", err, err)
		}
	})
	t.Run("仅墙钟超-原因是时间", func(t *testing.T) {
		clk := newFakeClock(20 * time.Millisecond)
		start := clk.now()
		caseLog(t, "双保护-仅墙钟触发", "墙钟50ms、条数预算100万，每检查点推进20ms",
			"Execute 返回 *DeadlineExceededError（时间到点）")
		dd := newDataTable(p)
		dd.In[0] = 1
		err := Execute(p, dd, 10, ExecConfig{
			MaxInstructions: 1_000_000, DeadlineMS: 50, start: start, now: clk.now,
		})
		var dl *DeadlineExceededError
		if !errors.As(err, &dl) {
			t.Fatalf("预期 *DeadlineExceededError，got %T %v", err, err)
		}
	})
	t.Run("同一指令边界两道同时到点-墙钟优先", func(t *testing.T) {
		clk := newFakeClock(100 * time.Millisecond)
		start := clk.now()
		caseLog(t, "双保护-同点触发", "墙钟50ms 与条数预算3 在同一检查点都满足",
			"先判墙钟，报 *DeadlineExceededError")
		dd := newDataTable(p)
		dd.In[0] = 1
		err := Execute(p, dd, 10, ExecConfig{
			MaxInstructions: 3, DeadlineMS: 50, start: start, now: clk.now,
		})
		var dl *DeadlineExceededError
		if !errors.As(err, &dl) {
			t.Fatalf("同点触发必须记墙钟原因，got %T %v", err, err)
		}
	})
	t.Run("条数先到-原因是条数", func(t *testing.T) {
		clk := newFakeClock(time.Millisecond)
		start := clk.now()
		caseLog(t, "双保护-条数先触发", "墙钟50ms，每检查点1ms，条数预算3",
			"报 *WatchdogError（时间未到点）")
		dd := newDataTable(p)
		dd.In[0] = 1
		err := Execute(p, dd, 10, ExecConfig{
			MaxInstructions: 3, DeadlineMS: 50, start: start, now: clk.now,
		})
		var wd *WatchdogError
		if !errors.As(err, &wd) {
			t.Fatalf("预期 *WatchdogError，got %T %v", err, err)
		}
	})
}

// TestWallClockDisabledByDefault 验证缺省配置墙钟保护关闭：死循环只受条数预算约束。
func TestWallClockDisabledByDefault(t *testing.T) {
	caseLog(t, "墙钟保护-缺省关闭", "DefaultConfig 不设 watchdog_time_ms",
		"WatchdogTimeMS=0；行为与旧版本完全一致")
	cfg := DefaultConfig()
	if cfg.WatchdogTimeMS != 0 {
		t.Fatalf("缺省必须为 0（关闭），got %d", cfg.WatchdogTimeMS)
	}
}
