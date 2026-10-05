package plc

import (
	"testing"
)

func TestScanStrictPhasesAndSnapshot(t *testing.T) {
	caseLogger(t, "扫描-严格三阶段/输入快照",
		"X0->Y0 直通程序；扫描进行到一半时现场线程把 X0 从 false 改成 true",
		"程序在该周期只能看到采样瞬间的值 false，Y0 到下一周期才变 true")

	src := "LD X0\nOUT Y0\nEND\n"
	cfg := testConfig(t)
	rt, io := newRuntime(t, src, cfg)

	io.SetInput(refOf("X0"), false)

	// 制造“采样之后、程序执行之前”的现场跳变：通过执行钩子。
	rt.SetExecHook(func(cycle int64) error {
		if cycle == 1 {
			io.SetInput(refOf("X0"), true) // 模拟周期中途输入更新
		}
		return nil
	})

	rt.ScanOnce()
	if got := io.LastOutput(refOf("Y0")); got != false {
		t.Fatalf("周期1 不应看到半更新输入，Y0=false，得到 %v", got)
	}
	rt.ScanOnce()
	if got := io.LastOutput(refOf("Y0")); got != true {
		t.Fatalf("周期2 应看到已稳定的新输入，Y0=true，得到 %v", got)
	}
}

func TestScanReproducible(t *testing.T) {
	caseLogger(t, "扫描-相同输入序列结果可复现",
		"启保停程序 + 两条完全相同的 12 周期输入序列，跑两个独立实例",
		"逐周期 Y0/Y1 必须逐拍相等")

	src := "LD X0\nSET Y0\nLD X1\nRST Y0\nLD Y0\nOUT Y1\nEND\n"
	seq := []struct {
		x0, x1 bool
	}{
		{false, false}, {false, false}, {true, false}, {false, false},
		{false, false}, {false, true}, {false, false}, {true, false},
		{false, false}, {false, false}, {false, true}, {false, false},
	}
	run := func() []bool {
		cfg := testConfig(t)
		rt, io := newRuntime(t, src, cfg)
		var res []bool
		for _, s := range seq {
			io.SetInput(refOf("X0"), s.x0)
			io.SetInput(refOf("X1"), s.x1)
			rt.ScanOnce()
			res = append(res, io.LastOutput(refOf("Y0")), io.LastOutput(refOf("Y1")))
		}
		return res
	}
	a, b := run(), run()
	if len(a) != len(b) {
		t.Fatal("长度不一致")
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("第 %d 拍结果不一致: %v vs %v", i/2, a, b)
		}
	}
}

func TestTimerDeterministic(t *testing.T) {
	caseLogger(t, "扫描-定时器确定行为",
		"TON T0 K30（30ms；周期 10ms，第 3 个使能周期到点）；随后断能一次",
		"前 2 周期 T0=0，第 3 周期起 T0=1/Y0=1；断能后立即清零")

	src := "LD X0\nTON T0 K30\nLD T0\nOUT Y0\nEND\n"
	cfg := testConfig(t, "T0")
	rt, io := newRuntime(t, src, cfg)

	io.SetInput(refOf("X0"), true)
	for i := 1; i <= 2; i++ {
		rt.ScanOnce()
		if io.LastOutput(refOf("Y0")) {
			t.Fatalf("第 %d 周期不应到点", i)
		}
	}
	rt.ScanOnce() // 第 3 周期：accum 30 >= 30
	if !io.LastOutput(refOf("Y0")) {
		t.Fatal("第 3 周期应到点")
	}
	io.SetInput(refOf("X0"), false)
	rt.ScanOnce()
	if io.LastOutput(refOf("Y0")) {
		t.Fatal("断能后定时器应复位")
	}
}

func TestCounterDeterministic(t *testing.T) {
	caseLogger(t, "扫描-计数器确定行为",
		"CTU C0 K3：X0 产生 3 个上升沿；X1 复位",
		"上升沿才计数，常按不多计；第 3 个上升沿 C0 完成；复位清零")

	src := "LD X0\nCTU C0 K3\nLD C0\nOUT Y0\nLD X1\nRST C0\nEND\n"
	cfg := testConfig(t, "C0")
	rt, io := newRuntime(t, src, cfg)

	// 3 个上升沿，每沿后再持续一拍（验证常按不多计）。
	for pulse := 0; pulse < 3; pulse++ {
		io.SetInput(refOf("X0"), true)
		rt.ScanOnce()
		io.SetInput(refOf("X0"), false)
		rt.ScanOnce()
	}
	if !io.LastOutput(refOf("Y0")) {
		t.Fatal("第 3 个上升沿后应完成")
	}
	// 再来一个上升沿不应改变完成态之外的计数（已完成保持 true）。
	io.SetInput(refOf("X0"), true)
	rt.ScanOnce()
	if !io.LastOutput(refOf("Y0")) {
		t.Fatal("完成位应保持")
	}
	io.SetInput(refOf("X0"), false)
	io.SetInput(refOf("X1"), true)
	rt.ScanOnce() // 复位拍：X0 已断开，RST C0 生效
	rt.ScanOnce() // 下一周期 C0 完成位为 0，Y0 输出清除
	if io.LastOutput(refOf("Y0")) {
		t.Fatal("复位次周期完成位与 Y0 应清除")
	}
	io.SetInput(refOf("X1"), false)
}

func TestEdgeDetection(t *testing.T) {
	caseLogger(t, "扫描-上升/下降沿检测",
		"LDP X0->Y0 与 LDF X0->Y1：X0 依次 off,on,on,off",
		"Y0 仅在 off->on 那一周期为 true，Y1 仅在 on->off 那一周期为 true")

	src := "LDP X0\nOUT Y0\nLDF X0\nOUT Y1\nEND\n"
	cfg := testConfig(t)
	rt, io := newRuntime(t, src, cfg)

	steps := []bool{false, true, true, false}
	wantUp := []bool{false, true, false, false}
	wantDown := []bool{false, false, false, true}
	for i, v := range steps {
		io.SetInput(refOf("X0"), v)
		rt.ScanOnce()
		if io.LastOutput(refOf("Y0")) != wantUp[i] {
			t.Fatalf("第 %d 周期上升沿结果错误: got %v want %v", i+1, io.LastOutput(refOf("Y0")), wantUp[i])
		}
		if io.LastOutput(refOf("Y1")) != wantDown[i] {
			t.Fatalf("第 %d 周期下降沿结果错误: got %v want %v", i+1, io.LastOutput(refOf("Y1")), wantDown[i])
		}
	}
}

func TestJMPConditional(t *testing.T) {
	caseLogger(t, "扫描-条件跳转",
		"X0 为真时跳过 M0 置位段；M0 再驱动 Y0",
		"X0=false 且 X1=true => M0/Y0=true（执行段）；X0=true => 段被跳过，M0 保持 false")

	src := "LD X2\nRST M0\nLD X0\nJMP skip\nLD X1\nOUT M0\nLBL skip\nLD M0\nOUT Y0\nEND\n"
	cfg := testConfig(t)
	rt, io := newRuntime(t, src, cfg)

	io.SetInput(refOf("X2"), true)
	io.SetInput(refOf("X0"), true)
	rt.ScanOnce() // 先清零 M0，证明初始 M0=false
	if io.LastOutput(refOf("Y0")) {
		t.Fatal("初始 Y0 应为 false")
	}
	io.SetInput(refOf("X1"), true)
	io.SetInput(refOf("X2"), false)
	io.SetInput(refOf("X0"), false)
	rt.ScanOnce()
	if !io.LastOutput(refOf("Y0")) {
		t.Fatal("未跳转时应执行 Y0=X1=true")
	}
	io.SetInput(refOf("X0"), true)
	io.SetInput(refOf("X2"), true)
	rt.ScanOnce()
	if io.LastOutput(refOf("Y0")) {
		t.Fatal("跳转后 M0 段被跳过，M0 保持 false，Y0=false")
	}
}
