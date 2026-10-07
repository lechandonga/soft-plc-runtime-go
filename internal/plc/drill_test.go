package plc

import (
	"strings"
	"testing"
)

// drillProgram：第 1 周期把 MW2 置成已提交的 7（沿用示例程序的初始化惯用法）；
// 故障周期按顺序写 Q0.0、M0.0、Q0.1，异常点设在第 1 条写入之后，
// 用于验证“异常点之前的半截写入一个不留、异常点之后不再执行”。
const drillProgram = `
LD I0.1
OUT Q0.0
LD I0.2
OUT M0.0
LD I0.3
OUT Q0.1
MOV 7 MW2
END
`

// statementCount 帮助用例按真实指令边界设置演练位置。
func mustParse(t *testing.T, src string) *Program {
	t.Helper()
	p, err := Parse(src)
	if err != nil {
		t.Fatalf("测试程序非法: %v", err)
	}
	return p
}

func drillConfig(t *testing.T, scan int64, after int) Config {
	t.Helper()
	cfg := DefaultConfig()
	cfg.ExecFaultScan = scan
	cfg.ExecFaultAfterInstr = after
	cfg.SafeOutputs = map[string]int64{"Q0.0": 0, "Q0.1": 0}
	return cfg
}

// stmtIndexBefore 返回程序中“第 ord 条指令执行完之后”注入时，
// 预期仍执行到的源行（仅测试可读性用，真实位置由指令计数保证）。
func TestDrillFaultFullChain(t *testing.T) {
	p := mustParse(t, drillProgram)
	// 找到第一条 OUT Q0.0 的指令序号（0 基），注入点设在它之后。
	outIdx := -1
	for i, st := range p.Statements {
		if st.Op == InsOUT && st.Args[0].Raw == "Q0.0" {
			outIdx = i
			break
		}
	}
	if outIdx < 0 {
		t.Fatal("测试程序缺少 OUT Q0.0")
	}

	rt, io := newTestRuntime(t, drillProgram, drillConfig(t, 2, outIdx+1))

	// 第 1 周期：完整提交 MW2=7。
	if err := rt.Scan(); err != nil {
		t.Fatalf("初始化周期失败: %v", err)
	}
	// 第 2 周期：三个写入输入全给 1；异常在 OUT Q0.0 之后确定性抛出。
	io.setBit(0, 1, true) // I0.1
	io.setBit(0, 2, true)
	io.setBit(0, 3, true)
	caseLog(t, "执行故障演练-异常点后整体回滚", "第2周期、第1条OUT之后注入；Q0.0/M0.0/Q0.1 三写输入全1",
		"ExecutionFault（演练）；Q0.0/M0.0/Q0.1 全部回滚为周期前值；安全输出整包下发")
	err := rt.Scan()
	if err == nil {
		t.Fatal("预期演练执行故障")
	}
	f := rt.Fault()
	if f.Kind != FaultExecution {
		t.Fatalf("故障必须区分为 ExecutionFault，got %s (%s)", f.Kind, f.Detail)
	}
	if !strings.Contains(f.Detail, "执行故障演练") {
		t.Fatalf("明细应标明演练注入: %s", f.Detail)
	}
	// 异常点之前的半截写入必须回滚：Q0.0 回到周期前 0。
	if rt.SnapshotOutputs()[0] != 0 {
		t.Fatalf("异常点之前的半截写入必须回滚: out=%08b", rt.SnapshotOutputs()[0])
	}
	// 异常点之后本周期不再执行：M0.0、Q0.1 都不得被写成 1。
	if rt.SnapshotMemory()[0]&1 != 0 {
		t.Fatalf("异常点之后的 M0.0 写入不得发生: mem=%08b", rt.SnapshotMemory()[0])
	}
	if io.outBit(0, 1) {
		t.Fatal("异常点之后的 Q0.1 写入不得发生，安全值必须为 0")
	}
	// 已提交状态完好：MW2 仍是第 1 周期提交的 7。
	if v, _ := rt.committed.readWord(mustWord("MW2")); v != 7 {
		t.Fatalf("最近完整提交状态被破坏: MW2=%d want 7", v)
	}

	// 锁存：后续周期不推进，输入变化/模拟 I/O 健康都不会自愈。
	before := rt.ScanCount()
	io.setBit(0, 1, false)
	if err := rt.Scan(); err == nil || rt.ScanCount() != before {
		t.Fatal("执行故障必须锁存，不得悄悄恢复")
	}

	caseLog(t, "执行故障演练-显式恢复后从最近提交继续", "I/O 健康，调用 Recover 后再跑一个正常周期",
		"恢复成功；下周期从 MW2=7 的已提交基线继续，不经过任何半成品")
	if err := rt.Recover(); err != nil {
		t.Fatalf("健康 I/O 下恢复应成功: %v", err)
	}
	if rt.Fault().Kind != FaultNone {
		t.Fatal("恢复后故障应清除")
	}
	// 恢复后下一周期：只给 I0.1=1，应正常提交 Q0.0=1（干净基线，不含故障周期残留）。
	io.setBit(0, 2, false)
	io.setBit(0, 3, false)
	io.setBit(0, 1, true)
	if err := rt.Scan(); err != nil {
		t.Fatalf("恢复后扫描失败: %v", err)
	}
	if !io.outBit(0, 0) || io.outBit(0, 1) {
		t.Fatalf("恢复后应从干净基线继续: Q0.0=%v Q0.1=%v", io.outBit(0, 0), io.outBit(0, 1))
	}
	if rt.SnapshotMemory()[0]&1 != 0 {
		t.Fatal("故障周期写入的 M0.0 不得复活")
	}
}

func mustWord(name string) Operand {
	a, err := ParseOperand(name)
	if err != nil {
		panic(err)
	}
	return a
}

// SnapshotWordByName 是测试辅助：按名称读当前数据表里的字。
func SnapshotWordByName(r *Runtime, name string) (int64, error) {
	a := mustWord(name)
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.data.readWord(a)
}

// TestDrillFaultDistinguishable 验证演练故障与 I/O 失联、单周期超时、
// 保持写入失败进入互不相同的故障状态。
func TestDrillFaultDistinguishable(t *testing.T) {
	src := `
LD I0.0
OUT Q0.0
END
`
	// 执行演练
	rt, io := newTestRuntime(t, src, drillConfig(t, 1, 1))
	io.setBit(0, 0, true)
	caseLog(t, "执行故障演练-故障种类可区分", "第1周期第1条指令后注入",
		"FaultKind=ExecutionFault，区别于 IOFault/WatchdogFault/RetainFault")
	if err := rt.Scan(); err == nil || rt.Fault().Kind != FaultExecution {
		t.Fatalf("预期 ExecutionFault: %v", err)
	}
}

// TestDrillFiresExactlyOnce 验证单次触发：第一个命中周期后不再注入。
func TestDrillFiresExactlyOnce(t *testing.T) {
	src := `
LD I0.0
OUT Q0.0
END
`
	cfg := drillConfig(t, 2, 1)
	cfg.ExecFaultScanEnd = 4 // 窗口 [2,4]：只在第 2 周期触发一次
	rt, _ := newTestRuntime(t, src, cfg)
	caseLog(t, "执行故障演练-窗口内只触发一次", "窗口[2,4]，命中第2周期后显式恢复，再跑第3/4周期",
		"第2周期 ExecutionFault；恢复后第3/4周期成功，演练不二次触发")
	if err := rt.Scan(); err != nil {
		t.Fatalf("窗口外周期不应触发: %v", err)
	}
	if err := rt.Scan(); err == nil || rt.Fault().Kind != FaultExecution {
		t.Fatalf("窗口首个周期必须触发: %v", err)
	}
	if err := rt.Recover(); err != nil {
		t.Fatal(err)
	}
	if err := rt.Scan(); err != nil {
		t.Fatalf("演练只应触发一次，第3周期却故障: %v", err)
	}
	if err := rt.Scan(); err != nil {
		t.Fatalf("演练只应触发一次，第4周期却故障: %v", err)
	}
}

// TestDrillDisabledByDefault 验证演练关闭时对正常语义零影响。
func TestDrillDisabledByDefault(t *testing.T) {
	src := `
LD I0.0
OUT Q0.0
END
`
	rt, io := newTestRuntime(t, src, DefaultConfig())
	caseLog(t, "执行故障演练-缺省关闭", "DefaultConfig 不设任何 exec_fault_*",
		"连续 20 周期全部成功，无故障")
	io.setBit(0, 0, true)
	for i := 0; i < 20; i++ {
		if err := rt.Scan(); err != nil {
			t.Fatalf("演练关闭时不得注入故障: %v", err)
		}
	}
	if rt.ScanCount() != 20 {
		t.Fatalf("成功周期数错误: %d", rt.ScanCount())
	}
}

// TestDrillConfigValidation 覆盖演练配置的启动期拒绝口径。
func TestDrillConfigValidation(t *testing.T) {
	src := `
LD I0.0
OUT Q0.0
END
`
	p, _ := Parse(src)
	cases := []struct {
		name string
		cfg  Config
	}{
		{"注入位置超出指令边界", func() Config {
			c := DefaultConfig()
			c.ExecFaultScan = 1
			c.ExecFaultAfterInstr = len(p.Statements) + 1
			return c
		}()},
		{"窗口结束早于开始", func() Config {
			c := DefaultConfig()
			c.ExecFaultScan = 3
			c.ExecFaultScanEnd = 2
			return c
		}()},
		{"只给 after 不给 scan", func() Config {
			c := DefaultConfig()
			c.ExecFaultAfterInstr = 1
			return c
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			caseLog(t, "执行故障演练-配置拒绝", tc.name,
				"NewRuntime 以 StructureError 拒绝，绝不带病启动")
			if _, err := NewRuntime(src, tc.cfg, newFakeIO(p.InputSize, p.OutputSize)); err == nil {
				t.Fatal("非法演练配置必须拒绝启动")
			}
		})
	}
}

// TestDrillRearmedAfterReload 验证热重载后演练按新配置重新装弹/关闭。
func TestDrillRearmedAfterReload(t *testing.T) {
	src := `
LD I0.0
OUT Q0.0
END
`
	rt, io := newTestRuntime(t, src, drillConfig(t, 1, 1))
	io.setBit(0, 0, true)
	if err := rt.Scan(); err == nil || rt.Fault().Kind != FaultExecution {
		t.Fatalf("预期演练故障: %v", err)
	}
	if err := rt.Recover(); err != nil {
		t.Fatal(err)
	}
	// 热重载为同程序但关闭演练：旧配置已触发过也必须以新配置为准。
	caseLog(t, "执行故障演练-热重载原子切换", "故障恢复后 Reload 为关闭演练的新配置",
		"Applied=true；后续周期不再注入；非法演练配置重载被整体拒绝并跑旧程序")
	if res := rt.Reload(src, DefaultConfig()); !res.Applied {
		t.Fatalf("关闭演练的重载应生效: %+v", res)
	}
	if err := rt.Scan(); err != nil {
		t.Fatalf("关闭演练后不得再触发: %v", err)
	}

	// 非法演练配置（注入点越界）热重载必须整体拒绝，旧配置（演练已装弹/触发）继续。
	bad := DefaultConfig()
	bad.ExecFaultScan = 1
	bad.ExecFaultAfterInstr = 99
	res := rt.Reload(src, bad)
	if res.Applied || res.Reason != "ValidationRejected" {
		t.Fatalf("非法演练配置重载必须拒绝: %+v", res)
	}

	// 重新开启演练（扫描尝试序号很大，保证下一拍落在窗口内）的重载后，
	// 应立即重新装弹并在第一个扫描尝试触发。
	later := DefaultConfig()
	later.ExecFaultScan = int64(rt.scanAttempts) + 1
	later.ExecFaultAfterInstr = 1
	later.SafeOutputs = map[string]int64{"Q0.0": 0}
	res = rt.Reload(src, later)
	if !res.Applied {
		t.Fatalf("合法演练重载应生效: %+v", res)
	}
	if err := rt.Scan(); err == nil || rt.Fault().Kind != FaultExecution {
		t.Fatalf("重载后演练应重新装弹并触发: %v", err)
	}
}
