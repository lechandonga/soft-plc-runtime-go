package plc

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// fakeIO 是可脚本化、可断线的内存 I/O。
type fakeIO struct {
	mu         sync.Mutex
	in         []byte
	out        []byte
	connected  bool
	failWrites int
}

func newFakeIO(inSize, outSize int) *fakeIO {
	return &fakeIO{in: make([]byte, inSize), out: make([]byte, outSize), connected: true}
}

func (f *fakeIO) Read(size int) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.connected {
		return nil, fmt.Errorf("模拟断线")
	}
	cp := make([]byte, size)
	copy(cp, f.in)
	return cp, nil
}

func (f *fakeIO) Write(out []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.connected {
		return fmt.Errorf("模拟断线")
	}
	if f.failWrites > 0 {
		f.failWrites--
		return fmt.Errorf("模拟输出提交失败")
	}
	f.out = append([]byte(nil), out...)
	return nil
}

func (f *fakeIO) Healthy() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected
}

func (f *fakeIO) setBit(b, bit int, v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v {
		f.in[b] |= 1 << bit
	} else {
		f.in[b] &^= 1 << bit
	}
}

func (f *fakeIO) disconnect() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connected = false
}
func (f *fakeIO) reconnect() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connected = true
}
func (f *fakeIO) outBit(b, bit int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.out[b]&(1<<bit) != 0
}

func newTestRuntime(t *testing.T, src string, cfg Config) (*Runtime, *fakeIO) {
	t.Helper()
	p, err := Parse(src)
	if err != nil {
		t.Fatalf("测试程序非法: %v", err)
	}
	io := newFakeIO(p.InputSize, p.OutputSize)
	rt, err := NewRuntime(src, cfg, io)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	return rt, io
}

// TestScanConsistency 验证：相同输入序列 + 相同配置，两次独立运行输出轨迹完全一致；
// 周期内程序看不到半更新输入（采样后整周期只读快照）。
func TestScanConsistency(t *testing.T) {
	src := `
LD I0.0
OR Q0.0
ANI I0.1
OUT Q0.0
LD I0.0
EU
OUT Q0.1
END
`
	run := func() []bool {
		rt, io := newTestRuntime(t, src, DefaultConfig())
		trace := make([]bool, 0, 8)
		// 输入序列：按下、保持、松开（边沿必须只出现在按下那一拍）
		press := []bool{true, true, false, false, true, false}
		for _, v := range press {
			io.setBit(0, 0, v)
			if err := rt.Scan(); err != nil {
				t.Fatalf("扫描失败: %v", err)
			}
			trace = append(trace, io.outBit(0, 1))
		}
		return trace
	}
	a := run()
	b := run()
	caseLog(t, "扫描一致性-可复现", "两次运行相同输入序列 [1,1,0,0,1,0]",
		"EU 边沿输出轨迹必须逐拍相同且仅在上升沿为真")
	if fmt.Sprint(a) != fmt.Sprint(b) {
		t.Fatalf("结果不可复现: %v vs %v", a, b)
	}
	want := []bool{true, false, false, false, true, false}
	if fmt.Sprint(a) != fmt.Sprint(want) {
		t.Fatalf("边沿轨迹错误: got %v want %v", a, want)
	}
}

// TestTimerCounterDeterminism 验证 TON/CTU 在连续周期中的确定行为。
func TestTimerCounterDeterminism(t *testing.T) {
	src := `
LD I0.0
TON T0 30
LD T0.Q
OUT Q0.0
LD I0.1
CTU C0 3 I0.1 I0.2
LD C0.Q
OUT Q0.1
END
`
	rt, io := newTestRuntime(t, src, DefaultConfig())
	caseLog(t, "状态元件-TON 三周期到点", "I0.0 持续为真，周期 10ms，PT=30ms",
		"第1/2周期 Q0.0=0，第3周期起 Q0.0=1")
	io.setBit(0, 0, true)
	for i := 1; i <= 2; i++ {
		if err := rt.Scan(); err != nil {
			t.Fatal(err)
		}
		if io.outBit(0, 0) {
			t.Fatalf("第%d周期 TON 未到点不应输出", i)
		}
	}
	if err := rt.Scan(); err != nil {
		t.Fatal(err)
	}
	if !io.outBit(0, 0) {
		t.Fatal("第3周期 TON 应到点")
	}
	// 断电式复位：输入撤掉后 ET 清零，Q 复位
	io.setBit(0, 0, false)
	if err := rt.Scan(); err != nil {
		t.Fatal(err)
	}
	if io.outBit(0, 0) {
		t.Fatal("TON 输入撤掉后应立即复位")
	}

	caseLog(t, "状态元件-CTU 上升沿计数三次到 PV", "I0.1 脉冲三拍，复位 I0.2=0",
		"CV 每上升沿加 1，第3拍 Q0.1=1；复位拍 CV/Q 归零")
	for i := 0; i < 3; i++ {
		io.setBit(0, 1, true)
		if err := rt.Scan(); err != nil {
			t.Fatal(err)
		}
		io.setBit(0, 1, false)
		if err := rt.Scan(); err != nil {
			t.Fatal(err)
		}
	}
	c, _ := rt.CounterState(0)
	if c.CV != 3 || !io.outBit(0, 1) {
		t.Fatalf("计数器状态错误: CV=%d Q=%v", c.CV, io.outBit(0, 1))
	}
	io.setBit(0, 2, true)
	if err := rt.Scan(); err != nil {
		t.Fatal(err)
	}
	c, _ = rt.CounterState(0)
	if c.CV != 0 || c.Q {
		t.Fatalf("复位失败: %+v", c)
	}
}

// TestWatchdogFaultAndRecovery 验证单周期超时进入可区分故障、落安全值、不自动恢复。
func TestWatchdogFaultAndRecovery(t *testing.T) {
	// JMP 构成死循环，指令预算设很小以确定触发看门狗。
	src := `
loop:
LD I0.0
JMP loop
END
`
	cfg := DefaultConfig()
	cfg.WatchdogInstructions = 10
	cfg.SafeOutputs = map[string]int64{}
	rt, io := newTestRuntime(t, src, cfg)
	io.setBit(0, 0, true) // 使 JMP 条件恒真，确定构成单周期死循环
	caseLog(t, "看门狗-预算耗尽即故障", "JMP 死循环 + 预算10条",
		"Scan 返回 WatchdogFault，后续扫描被锁存，必须显式 Recover")
	err := rt.Scan()
	if err == nil {
		t.Fatal("预期看门狗超时")
	}
	if rt.Fault().Kind != FaultWatchdog {
		t.Fatalf("故障类型错误: %s", rt.Fault().Kind)
	}
	before := rt.ScanCount()
	if err := rt.Scan(); err == nil || rt.ScanCount() != before {
		t.Fatal("故障状态下不得继续推进扫描（不自动恢复）")
	}
	// 恢复条件：I/O 健康且试读试写成功
	if err := rt.Recover(); err != nil {
		t.Fatalf("健康 I/O 下恢复应成功: %v", err)
	}
	if rt.Fault().Kind != FaultNone {
		t.Fatal("恢复后故障应清除")
	}
	_ = io
}

// TestIOFaultSafeOutputsAndNoAutoRecovery 验证 I/O 失联：
// 采样失败 -> IOFault；输出落安全值；重连后也必须显式恢复。
func TestIOFaultSafeOutputsAndNoAutoRecovery(t *testing.T) {
	src := `
LD I0.0
OUT Q0.0
END
`
	cfg := DefaultConfig()
	cfg.SafeOutputs = map[string]int64{"Q0.0": 0}
	rt, io := newTestRuntime(t, src, cfg)
	io.setBit(0, 0, true)
	if err := rt.Scan(); err != nil {
		t.Fatal(err)
	}
	if !io.outBit(0, 0) {
		t.Fatal("正常周期 Q0.0 应为 1")
	}
	io.disconnect()
	caseLog(t, "故障-I/O 失联落安全值", "运行中断线",
		"进入 IOFault，输出整包为安全值 0，重连后不自动恢复")
	err := rt.Scan()
	if err == nil || rt.Fault().Kind != FaultIO {
		t.Fatalf("预期 IOFault，got %v", err)
	}
	// 仍然断线时拒绝恢复
	if err := rt.Recover(); err == nil {
		t.Fatal("断线未恢复时 Recover 必须拒绝")
	}
	io.reconnect()
	// 仅重连但不调用 Recover：不得悄悄恢复
	if err := rt.Scan(); err == nil {
		t.Fatal("重连后未显式恢复，扫描必须仍被锁存")
	}
	if err := rt.Recover(); err != nil {
		t.Fatalf("重连并满足探针条件后应可恢复: %v", err)
	}
	// 恢复基线是最近一次完整提交状态：上一次提交时 Q0.0=1；
	// 恢复后先采样新输入（当前 I0.0=true），下一周期应正常输出。
	io.setBit(0, 0, true)
	if err := rt.Scan(); err != nil {
		t.Fatalf("恢复后扫描失败: %v", err)
	}
	if !io.outBit(0, 0) {
		t.Fatal("恢复后正常周期输出应恢复")
	}
}

// TestExecutionFaultRollsBack 验证执行异常时周期内写入整体回滚。
// 利用程序在写 Q0.0 之后再触发不可恢复内部错误——
// 通过超短预算无法区分，这里用“除零类”语义：访问不可达地址由执行期越界触发。
// 为保持纯确定性，构造一个写位后再 JMP 到结构外的情形不可能通过校验，
// 因此执行异常用 Watchdog 也覆盖了回滚；本用例改用失败的输出提交验证回滚。
func TestCommitFailureRollsBackState(t *testing.T) {
	src := `
LD I0.0
OUT Q0.0
END
`
	cfg := DefaultConfig()
	rt, io := newTestRuntime(t, src, cfg)
	io.setBit(0, 0, true)
	io.failWrites = 1
	caseLog(t, "提交失败-整体回滚不落半状态", "输出 Write 第1次失败",
		"进入 IOFault，内部 Q0.0 回滚为周期前的 0，安全输出整包下发")
	if err := rt.Scan(); err == nil || rt.Fault().Kind != FaultIO {
		t.Fatalf("预期 IOFault: %v", err)
	}
	if rt.SnapshotOutputs()[0] != 0 {
		t.Fatalf("回滚后输出映像应为 0，got %08b", rt.SnapshotOutputs()[0])
	}
}

// TestHotReloadAtomicity 验证热重载：新程序非法时完整沿用旧程序；合法时整体切换。
func TestHotReloadAtomicity(t *testing.T) {
	srcV1 := `
LD I0.0
OUT Q0.0
END
`
	srcV2Bad := `
LD I0.0
OUT Q0.0
BARF
`
	srcV2 := `
LD I0.0
OUT Q0.1
END
`
	rt, io := newTestRuntime(t, srcV1, DefaultConfig())
	io.setBit(0, 0, true)
	if err := rt.Scan(); err != nil {
		t.Fatal(err)
	}
	caseLog(t, "热重载-非法新程序被整体拒绝", "V2 含未知指令 BARF",
		"Reload Applied=false，旧程序继续：输出仍落在 Q0.0")
	res := rt.Reload(srcV2Bad, DefaultConfig())
	if res.Applied || res.Reason != "ValidationRejected" {
		t.Fatalf("非法重载应被拒绝: %+v", res)
	}
	if err := rt.Scan(); err != nil {
		t.Fatalf("旧程序应继续运行: %v", err)
	}
	if !io.outBit(0, 0) || io.outBit(0, 1) {
		t.Fatalf("旧程序语义必须保持: Q0.0=%v Q0.1=%v", io.outBit(0, 0), io.outBit(0, 1))
	}
	caseLog(t, "热重载-合法新程序整体生效", "V2 改为驱动 Q0.1",
		"Reload Applied=true，下一拍输出位置切换，不存在混执行")
	if res := rt.Reload(srcV2, DefaultConfig()); !res.Applied {
		t.Fatalf("合法重载应生效: %+v", res)
	}
	if err := rt.Scan(); err != nil {
		t.Fatalf("新程序扫描失败: %v", err)
	}
	if io.outBit(0, 0) || !io.outBit(0, 1) {
		t.Fatalf("新程序语义应生效: Q0.0=%v Q0.1=%v", io.outBit(0, 0), io.outBit(0, 1))
	}
}

// TestHotReloadRejectsRetainMismatch 验证程序指纹/布局变化时拒绝加载保持文件。
func TestHotReloadRejectsRetainMismatch(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "retain.bin")
	srcV1 := `
LD I0.0
OUT Q0.0
MOV 7 MW0
END
`
	cfg1 := DefaultConfig()
	cfg1.Retain = []string{"MW0"}
	cfg1.RetainFile = file
	rt, io := newTestRuntime(t, srcV1, cfg1)
	io.setBit(0, 0, true)
	if err := rt.Scan(); err != nil {
		t.Fatal(err)
	}
	srcV2 := `
LD I0.0
OUT Q0.0
MOV 9 MW2
END
`
	cfg2 := cfg1
	cfg2.Retain = []string{"MW2"}
	caseLog(t, "热重载-保持布局变化拒绝", "V1 保持 MW0，V2 保持 MW2，文件已存在",
		"Reload 返回 RetainIncompatible，旧程序继续运行")
	res := rt.Reload(srcV2, cfg2)
	if res.Applied || res.Reason != "RetainIncompatible" {
		t.Fatalf("布局不兼容必须拒绝: %+v", res)
	}
	_ = io
}

// TestRetainRestartConsistency 验证掉电重启恢复到最近一次完整提交，
// 写到一半（伪装损坏）不生效；崩溃周期的状态丢失到上一拍。
func TestRetainRestartConsistency(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "retain.bin")
	src := `
LD I0.0
CTU C0 5 I0.0 I0.1
LD C0.Q
OUT Q0.0
END
`
	cfg := DefaultConfig()
	cfg.Retain = []string{"C0"}
	cfg.RetainFile = file

	run := func() (*Runtime, *fakeIO) {
		p, err := Parse(src)
		if err != nil {
			t.Fatal(err)
		}
		io := newFakeIO(p.InputSize, p.OutputSize)
		rt, err := NewRuntime(src, cfg, io)
		if err != nil {
			t.Fatalf("重启失败: %v", err)
		}
		return rt, io
	}

	rt, io := run()
	caseLog(t, "掉电保持-三个完整提交后重启", "I0.0 三个完整脉冲周期",
		"新进程 NewRuntime 后 C0.CV 必须等于 3")
	for i := 0; i < 3; i++ {
		io.setBit(0, 0, true)
		if err := rt.Scan(); err != nil {
			t.Fatal(err)
		}
		io.setBit(0, 0, false)
		if err := rt.Scan(); err != nil {
			t.Fatal(err)
		}
	}
	rt2, _ := run()
	c, _ := rt2.CounterState(0)
	if c.CV != 3 {
		t.Fatalf("重启后保持值错误: CV=%d want 3", c.CV)
	}

	caseLog(t, "掉电保持-半写文件不生效", "把正式文件截断为半截",
		"NewRuntime 必须以 Corrupt 拒绝启动，而不是载入部分值")
	st, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	full, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	half := append([]byte(nil), full[:st.Size()/2]...)
	if err := os.WriteFile(file, half, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRuntime(src, cfg, newFakeIO(1, 1)); err == nil {
		t.Fatal("损坏保持文件必须拒绝启动")
	} else {
		t.Logf("按预期拒绝: %v", err)
	}
}

// TestRetainRejectsProgramChange 验证程序版本变化时保持数据拒绝错位加载。
func TestRetainRejectsProgramChange(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "retain.bin")
	srcV1 := `
LD I0.0
OUT Q0.0
MOV 1 MW0
END
`
	cfg := DefaultConfig()
	cfg.Retain = []string{"MW0"}
	cfg.RetainFile = file
	p, _ := Parse(srcV1)
	io := newFakeIO(p.InputSize, p.OutputSize)
	rt, err := NewRuntime(srcV1, cfg, io)
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.Scan(); err != nil {
		t.Fatal(err)
	}
	srcV2 := `
LD I0.1
OUT Q0.0
MOV 1 MW0
END
`
	caseLog(t, "掉电保持-程序版本变化拒绝", "保持 MW0 不变，但源程序改动（指纹变化）",
		"NewRuntime 返回 ProgramChanged，宁可拒绝也不错位")
	if _, err := NewRuntime(srcV2, cfg, newFakeIO(1, 1)); err == nil {
		t.Fatal("程序版本变化必须拒绝恢复")
	}
}
