package plc

import (
	"errors"
	"testing"
	"time"
)

func TestWatchdogByInstructionBudget(t *testing.T) {
	caseLogger(t, "故障-单周期看门狗(指令预算)",
		"JMP 自循环程序，instruction_budget=10",
		"第 1 周期即进入 WATCHDOG 故障；故障期间扫描停止推进；无显式 Reset 不恢复")

	src := "LBL loop\nLD X0\nJMP loop\nEND\n"
	cfg := testConfig(t)
	cfg.InstructionBudget = 10
	rt, io := newRuntime(t, src, cfg)
	io.SetInput(refOf("X0"), true)

	rt.ScanOnce()
	f := rt.Fault()
	if f == nil || f.Code != FaultWatchdog {
		t.Fatalf("应进入 WATCHDOG 故障，得到 %+v", f)
	}
	if !errors.Is(asErr(f), ErrWatchdog) {
		t.Fatal("故障原因应 errors.Is ErrWatchdog")
	}
	// 安全值：Y0 未配置为 true，应为 false；配置 Y0=true 的安全点检查见另例。
	cycleAfterFault := rt.Cycle()
	rt.ScanOnce()
	if rt.Cycle() != cycleAfterFault {
		t.Fatal("故障态下扫描不得继续推进周期")
	}

	// WATCHDOG 必须人工显式复位才恢复（条件：确认即可），之后周期继续。
	if err := rt.Reset(); err != nil {
		t.Fatalf("人工复位应成功: %v", err)
	}
	if rt.Fault() != nil {
		t.Fatal("复位后应无故障")
	}
	// 复位后程序仍是同一死循环，下一周期再次看门狗——证明不是悄悄换程序。
	rt.ScanOnce()
	if rt.Fault() == nil {
		t.Fatal("同一超限程序复位后再扫描应再次看门狗")
	}
}

func TestWatchdogByWallTime(t *testing.T) {
	caseLogger(t, "故障-单周期看门狗(墙钟时间)",
		"watchdog_ms=1，执行钩子里 sleep 20ms",
		"命中墙钟检查点，进入 WATCHDOG 故障")

	src := "LD X0\nOUT Y0\n" + longRungs(1000) + "END\n"
	cfg := testConfig(t)
	cfg.WatchdogMillis = 1
	cfg.InstructionBudget = 1_000_000
	rt, _ := newRuntime(t, src, cfg)
	rt.SetExecHook(func(int64) error {
		time.Sleep(20 * time.Millisecond)
		return nil
	})
	rt.ScanOnce()
	if f := rt.Fault(); f == nil || f.Code != FaultWatchdog {
		t.Fatalf("应墙钟超时，得到 %+v", f)
	}
}

func longRungs(n int) string {
	s := ""
	for i := 0; i < n; i++ {
		s += "LD X0\nANI X1\n"
	}
	return s
}

func asErr(f *Fault) error { return f }

func TestExecException(t *testing.T) {
	caseLogger(t, "故障-执行异常",
		"执行钩子在第 3 周期返回普通错误",
		"前 2 周期正常，第 3 周期进入 EXEC 故障，输出落安全值")

	src := "LD X0\nOUT Y0\nEND\n"
	cfg := testConfig(t)
	cfg.SafeOutputs = map[string]bool{"Y0": false}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	rt, io := newRuntime(t, src, cfg)
	io.SetInput(refOf("X0"), true)
	rt.SetExecHook(func(cycle int64) error {
		if cycle == 3 {
			return errors.New("除零")
		}
		return nil
	})
	rt.ScanOnce()
	rt.ScanOnce()
	if io.LastOutput(refOf("Y0")) != true {
		t.Fatal("故障前 Y0 应跟随 X0=true")
	}
	rt.ScanOnce()
	f := rt.Fault()
	if f == nil || f.Code != FaultExec || f.Cycle != 3 {
		t.Fatalf("应在周期3进入 EXEC 故障，得到 %+v", f)
	}
	if io.LastOutput(refOf("Y0")) != false {
		t.Fatal("EXEC 故障后 Y0 必须落到安全值 false")
	}
	if err := rt.Reset(); err != nil {
		t.Fatalf("人工复位应成功: %v", err)
	}
}

func TestSafeOutputsConfigurable(t *testing.T) {
	caseLogger(t, "故障-安全值可配置",
		"safe_outputs 配置 Y0=true、Y1=false；I/O 读失败进故障",
		"故障瞬间提交的快照中 Y0=true、Y1=false")

	src := "LD X0\nOUT Y0\nLD X1\nOUT Y1\nEND\n"
	cfg := testConfig(t)
	rt, io := newRuntime(t, src, cfg)
	rt.ScanOnce() // 正常周期，提交过 Y0/Y1
	io.FailNext(1, 0)
	rt.ScanOnce()
	f := rt.Fault()
	if f == nil || f.Code != FaultIO {
		t.Fatalf("应进入 IO_LOST，得到 %+v", f)
	}
	if !io.LastOutput(refOf("Y0")) {
		t.Fatal("Y0 配置的安全值为 true")
	}
	if io.LastOutput(refOf("Y1")) {
		t.Fatal("Y1 默认安全值应为 false")
	}
}

func TestIOLostRecoveryRequiresHealthyIO(t *testing.T) {
	caseLogger(t, "故障-I/O 失联恢复条件",
		"模拟 I/O 掉线 -> IO_LOST；未恢复时 Reset 必须被拒绝；恢复后 Reset 成功",
		"拒绝时故障保持；读+写探测均通过才允许清除")

	src := "LD X0\nOUT Y0\nEND\n"
	cfg := testConfig(t)
	cfg.SafeOutputs = map[string]bool{"Y0": false}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	rt, io := newRuntime(t, src, cfg)
	io.SetOffline(true)
	rt.ScanOnce()
	if rt.Fault() == nil || rt.Fault().Code != FaultIO {
		t.Fatal("掉线应进入 IO_LOST")
	}
	if err := rt.Reset(); !errors.Is(err, ErrIOLost) {
		t.Fatalf("I/O 仍掉线时 Reset 必须被拒绝(ErrIOLost)，得到 %v", err)
	}
	if rt.Fault() == nil {
		t.Fatal("被拒绝的复位不得清除故障")
	}
	io.SetOffline(false)
	if err := rt.Reset(); err != nil {
		t.Fatalf("I/O 恢复后 Reset 应成功: %v", err)
	}

	// 写通道单独故障也要拦住复位。
	io.SetOffline(true)
	rt.ScanOnce()
	io.SetOffline(false)
	io.FailNext(0, 1)
	if err := rt.Reset(); !errors.Is(err, ErrIOLost) {
		t.Fatalf("写探测失败时 Reset 必须被拒绝，得到 %v", err)
	}
	if err := rt.Reset(); err != nil {
		t.Fatalf("探测恢复后应成功: %v", err)
	}
}

func TestNoAutoRecoveryAcrossScans(t *testing.T) {
	caseLogger(t, "故障-不得悄悄自动恢复",
		"IO 掉线一周期后立即恢复，连续 ScanOnce 5 次",
		"无 Reset 时故障必须始终存在，输出保持安全值")

	src := "LD X0\nOUT Y0\nEND\n"
	cfg := testConfig(t)
	cfg.SafeOutputs = map[string]bool{"Y0": false}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	rt, io := newRuntime(t, src, cfg)
	io.SetInput(refOf("X0"), true)
	rt.ScanOnce()
	io.FailNext(1, 0)
	rt.ScanOnce()
	if rt.Fault() == nil {
		t.Fatal("应已进入故障")
	}
	for i := 0; i < 5; i++ {
		rt.ScanOnce()
		if rt.Fault() == nil {
			t.Fatalf("第 %d 个后续周期发生了未授权的自动恢复", i)
		}
		if io.LastOutput(refOf("Y0")) != false {
			t.Fatal("故障期间输出必须维持安全值")
		}
	}
}
