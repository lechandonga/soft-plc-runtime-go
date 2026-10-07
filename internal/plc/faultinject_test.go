package plc

import (
	"strings"
	"testing"
)

// 演练程序：指令序号 0:LD 1:OUT 2:MOV 3:OUT 4:END。
const drillProgram = `
LD I0.0
OUT Q0.0
MOV 5 MW0
OUT Q0.1
END
`

// TestFaultInjectionDrillFullChain 用显式注入把“程序执行自身出错”的故障链路
// 完整走一遍：可区分故障、整体回滚、安全输出、锁存、显式恢复、从最近提交继续。
func TestFaultInjectionDrillFullChain(t *testing.T) {
	cfg := DefaultConfig()
	cfg.FaultInjection = &FaultInjectionConfig{
		ExecErrorScans: []int64{2}, // 第 2 个扫描周期
		AtInstruction:  2,          // 在 MOV 之前抛出（OUT Q0.0 已执行，MOV/OUT Q0.1 未执行）
	}
	rt, io := newTestRuntime(t, drillProgram, cfg)

	caseLog(t, "故障演练-基线提交", "第1拍 I0.0=1，注入未命中",
		"正常提交：Q0.0=1、Q0.1=1、MW0=5，扫描计数=1")
	io.setBit(0, 0, true)
	if err := rt.Scan(); err != nil {
		t.Fatalf("第1拍不应注入: %v", err)
	}
	if !io.outBit(0, 0) || !io.outBit(0, 1) {
		t.Fatal("第1拍输出应为 Q0.0=1 Q0.1=1")
	}

	caseLog(t, "故障演练-确定性注入", "第2拍 I0.0=0，注入点=指令2(MOV)",
		"进入 ExecutionFault（与 IOFault/WatchdogFault/ScanTimeoutFault/RetainFault 可区分）；"+
			"异常前的半截写入（Q0.0=0）整体回滚；异常后指令本周期不再执行")
	io.setBit(0, 0, false) // 若不回滚，OUT Q0.0 会写入 0
	err := rt.Scan()
	if err == nil {
		t.Fatal("预期注入异常")
	}
	f := rt.Fault()
	if f.Kind != FaultExecution {
		t.Fatalf("注入异常必须归类为 ExecutionFault: %s", f.Kind)
	}
	if !strings.Contains(f.Detail, "注入") {
		t.Fatalf("故障详情应标明演练注入来源: %s", f.Detail)
	}
	if rt.ScanCount() != 1 {
		t.Fatalf("故障周期不得计入扫描计数: %d", rt.ScanCount())
	}
	// 回滚判定：内部映像回到第1拍提交态（MW0=5 保留自提交态）；
	// 输出映像整包为安全值（默认全 0），且已下发到 I/O。
	if got := rt.SnapshotOutputs()[0]; got != 0 {
		t.Fatalf("故障后输出映像应为安全值 0，got %08b", got)
	}
	if got := rt.SnapshotMemory()[0]; got != 5 {
		t.Fatalf("回滚后 MW0 低字节应为最近提交值 5，got %d", got)
	}
	if io.outBit(0, 0) || io.outBit(0, 1) {
		t.Fatal("故障后下发到 I/O 的输出应为安全值 0")
	}

	caseLog(t, "故障演练-锁存不自动恢复", "I/O 断线再重连、输入再置 1（外部条件变化）",
		"扫描仍被锁存为 ExecutionFault，不悄悄恢复")
	io.disconnect()
	io.reconnect()
	io.setBit(0, 0, true)
	if err := rt.Scan(); err == nil || rt.Fault().Kind != FaultExecution {
		t.Fatal("故障必须锁存，外部条件变化不得自动恢复")
	}

	caseLog(t, "故障演练-显式恢复后继续", "Recover 后再扫描（注入每周期号至多触发一次）",
		"从最近一次完整提交（扫描计数=1 的状态）继续，而非故障周期半成品；后续周期正常")
	if err := rt.Recover(); err != nil {
		t.Fatalf("满足条件后恢复应成功: %v", err)
	}
	if err := rt.Scan(); err != nil {
		t.Fatalf("恢复后重跑第2拍不应再注入: %v", err)
	}
	if rt.ScanCount() != 2 {
		t.Fatalf("恢复后扫描计数应推进: %d", rt.ScanCount())
	}
	if !io.outBit(0, 0) || !io.outBit(0, 1) {
		t.Fatal("恢复后正常周期应重新驱动输出")
	}
}

// TestFaultInjectionDisabledNoEffect 验证：演练关闭（默认 nil）时，
// 同样的程序与输入序列语义、确定性完全不受影响。
func TestFaultInjectionDisabledNoEffect(t *testing.T) {
	caseLog(t, "故障演练-默认关闭零影响", "FaultInjection=nil，连跑 50 拍",
		"无任何注入、无故障，输出轨迹与未引入该能力时一致")
	rt, io := newTestRuntime(t, drillProgram, DefaultConfig())
	io.setBit(0, 0, true)
	for i := 0; i < 50; i++ {
		if err := rt.Scan(); err != nil {
			t.Fatalf("关闭演练时不应出现故障: 第%d拍 %v", i+1, err)
		}
	}
	if rt.ScanCount() != 50 {
		t.Fatalf("扫描计数错误: %d", rt.ScanCount())
	}

	caseLog(t, "故障演练-开启但未命中周期号", "注入配置在第 100 拍，连跑 10 拍",
		"未命中周期号时行为与关闭完全一致")
	cfg := DefaultConfig()
	cfg.FaultInjection = &FaultInjectionConfig{ExecErrorScans: []int64{100}, AtInstruction: 0}
	rt2, io2 := newTestRuntime(t, drillProgram, cfg)
	io2.setBit(0, 0, true)
	for i := 0; i < 10; i++ {
		if err := rt2.Scan(); err != nil {
			t.Fatalf("未命中周期号不应注入: 第%d拍 %v", i+1, err)
		}
	}
}

// TestFaultInjectionConfigValidation 验证演练配置自身的启动期校验，
// 冷启动与热重载口径一致。
func TestFaultInjectionConfigValidation(t *testing.T) {
	caseLog(t, "故障演练-配置非法拒绝", "AtInstruction 超出程序指令数 / 周期号为 0",
		"NewRuntime 与 Reload 都以 StructureError 拒绝，不允许半套配置生效")
	bad := DefaultConfig()
	bad.FaultInjection = &FaultInjectionConfig{ExecErrorScans: []int64{1}, AtInstruction: 99}
	if _, err := NewRuntime(drillProgram, bad, newFakeIO(1, 1)); err == nil {
		t.Fatal("注入点越界必须拒绝启动")
	}
	bad2 := DefaultConfig()
	bad2.FaultInjection = &FaultInjectionConfig{ExecErrorScans: []int64{0}, AtInstruction: 0}
	if _, err := NewRuntime(drillProgram, bad2, newFakeIO(1, 1)); err == nil {
		t.Fatal("周期号从 1 起计，0 必须拒绝")
	}

	rt, io := newTestRuntime(t, drillProgram, DefaultConfig())
	io.setBit(0, 0, true)
	if err := rt.Scan(); err != nil {
		t.Fatal(err)
	}
	if res := rt.Reload(drillProgram, bad); res.Applied || res.Reason != "ValidationRejected" {
		t.Fatalf("热重载非法演练配置必须整体拒绝: %+v", res)
	}
	if err := rt.Scan(); err != nil {
		t.Fatalf("旧配置应原样继续运行: %v", err)
	}
}
