package plc

import "testing"

// TestWriteTargetRejectionMatrix 覆盖“写入指令 × 目标对象”组合：
// 只读对象 → IllegalAddress；运行时自维护状态量/对象种类不符 → IllegalTarget；
// 位字用错仍按既有 TypeError 分类。全部在加载阶段拒绝，绝不到执行期才暴露。
func TestWriteTargetRejectionMatrix(t *testing.T) {
	cases := []struct {
		business string
		line     string // 单行写入指令（测试补齐前置 LD 与 END）
		reason   string
	}{
		// OUT（位写入）
		{"OUT-输入区只读", "OUT I0.1", "IllegalAddress"},
		{"OUT-立即数不是位对象", "OUT 1", "TypeError"},
		{"OUT-定时器到点位是运行时状态", "OUT T0.Q", "IllegalTarget"},
		{"OUT-计数器完成位是运行时状态", "OUT C0.Q", "IllegalTarget"},
		{"OUT-整个定时器元件不是位对象", "OUT T0", "TypeError"},
		{"OUT-整个计数器元件不是位对象", "OUT C0", "TypeError"},
		{"OUT-字地址不是位对象", "OUT MW2", "TypeError"},
		// SET（位写入，口径同 OUT）
		{"SET-输入区只读", "SET I0.1", "IllegalAddress"},
		{"SET-定时器字段不可写", "SET T0.Q", "IllegalTarget"},
		{"SET-计数器字段不可写", "SET C0.Q", "IllegalTarget"},
		// RST（位/字/整个计数器）
		{"RST-输入区只读", "RST I0.1", "IllegalAddress"},
		{"RST-立即数不可写", "RST 5", "IllegalAddress"},
		{"RST-定时器元件由运行时维护", "RST T0", "IllegalTarget"},
		{"RST-定时器字段由运行时维护", "RST T0.ET", "IllegalTarget"},
		{"RST-计数器字段不可单独写", "RST C0.CV", "IllegalTarget"},
		{"RST-计数器完成位不可写", "RST C0.Q", "IllegalTarget"},
		// MOV（字写入）
		{"MOV-输入字只读", "MOV 1 IW0", "IllegalAddress"},
		{"MOV-立即数目标不是字对象", "MOV 1 2", "TypeError"},
		{"MOV-定时器现值字段不可写", "MOV 1 T0.ET", "IllegalTarget"},
		{"MOV-定时器预设字段不可写", "MOV 1 T0.PT", "IllegalTarget"},
		{"MOV-计数器当前值字段不可写", "MOV 1 C0.CV", "IllegalTarget"},
		{"MOV-位地址不是字对象", "MOV 1 M0.0", "TypeError"},
	}
	for _, tc := range cases {
		t.Run(tc.business, func(t *testing.T) {
			src := "LD I0.0\n" + tc.line + "\nEND\n"
			caseLog(t, "写目标校验-"+tc.business, tc.line,
				"Parse/NewRuntime/Reload 三路径均按 "+tc.reason+" 拒绝，不产生部分加载")
			_, err := Parse(src)
			if err == nil {
				t.Fatalf("预期加载被拒绝(%s)，但成功", tc.reason)
			}
			le, ok := err.(*LoadError)
			if !ok {
				t.Fatalf("错误类型应为 *LoadError，got %T", err)
			}
			if le.Reason != tc.reason {
				t.Fatalf("拒绝分类错误: got %s want %s (%v)", le.Reason, tc.reason, le)
			}
		})
	}
}

// TestWriteTargetAcceptanceMatrix 验证对象种类对得上的组合仍然放行，
// 防止校验把正常写入误杀（含 RST Cn 整体复位）。
func TestWriteTargetAcceptanceMatrix(t *testing.T) {
	lines := []string{
		"OUT Q0.0",
		"OUT M0.0",
		"SET Q0.1",
		"SET M0.1",
		"RST Q0.0",
		"RST M0.0",
		"RST MW2",
		"RST C0",
		"MOV 1 MW2",
		"MOV MW2 QW0",
	}
	for _, ln := range lines {
		t.Run(ln, func(t *testing.T) {
			src := "LD I0.0\n" + ln + "\nEND\n"
			caseLog(t, "写目标校验-合法组合放行", ln,
				"Parse 成功；程序可冷启动")
			if _, err := Parse(src); err != nil {
				t.Fatalf("合法写入组合被误拒: %v", err)
			}
		})
	}

	// RST Cn 整体复位在冷启动下必须能真正跑起来（执行器支持，加载不得误杀）。
	src := `
LD I0.0
CTU C0 2 I0.0 I0.1
LD I0.2
RST C0
END
`
	p, err := Parse(src)
	if err != nil {
		t.Fatalf("RST C0 应通过加载: %v", err)
	}
	rt, err := NewRuntime(src, DefaultConfig(), newFakeIO(p.InputSize, p.OutputSize))
	if err != nil {
		t.Fatalf("含 RST C0 的程序冷启动失败: %v", err)
	}
	caseLog(t, "写目标校验-RST Cn 冷启动可执行", "CTU + RST C0 完整程序",
		"NewRuntime 成功；两个上升沿计数后整体复位 CV=0")
	io := rt.io.(*fakeIO)
	io.setBit(0, 0, true)
	rt.Scan()
	io.setBit(0, 0, false)
	rt.Scan()
	io.setBit(0, 0, true)
	rt.Scan()
	io.setBit(0, 0, false)
	rt.Scan()
	c, _ := rt.CounterState(0)
	if c.CV != 2 || !c.Q {
		t.Fatalf("计数前置错误: %+v", c)
	}
	io.setBit(0, 0, false)
	io.setBit(0, 2, true)
	if err := rt.Scan(); err != nil {
		t.Fatalf("RST C0 周期失败: %v", err)
	}
	c, _ = rt.CounterState(0)
	if c.CV != 0 || c.Q {
		t.Fatalf("RST C0 整体复位失败: %+v", c)
	}
}

// TestWriteTargetRejectedOnColdStartAndReload 验证冷启动与热重载口径一致：
// 非法写目标的程序冷启动直接拒绝；热重载整体拒绝并原样继续跑旧程序。
func TestWriteTargetRejectedOnColdStartAndReload(t *testing.T) {
	good := `
LD I0.0
OUT Q0.0
END
`
	bad := `
LD I0.0
OUT T0.Q
END
`
	p, err := Parse(good)
	if err != nil {
		t.Fatal(err)
	}
	io := newFakeIO(p.InputSize, p.OutputSize)

	// 冷启动路径
	caseLog(t, "写目标校验-冷启动拒绝", "OUT T0.Q（运行时状态量）",
		"NewRuntime 返回 IllegalTarget，程序完全不加载")
	if _, err := NewRuntime(bad, DefaultConfig(), newFakeIO(1, 1)); err == nil {
		t.Fatal("非法写目标程序冷启动必须拒绝")
	} else {
		if le, ok := err.(*LoadError); !ok || le.Reason != "IllegalTarget" {
			t.Fatalf("冷启动拒绝分类错误: %v", err)
		}
	}

	// 热重载路径：旧程序先跑一拍，非法新程序必须整体拒绝、旧语义不变。
	rt, err := NewRuntime(good, DefaultConfig(), io)
	if err != nil {
		t.Fatal(err)
	}
	io.setBit(0, 0, true)
	if err := rt.Scan(); err != nil {
		t.Fatal(err)
	}
	caseLog(t, "写目标校验-热重载整体拒绝", "运行中 Reload OUT T0.Q",
		"Applied=false/ValidationRejected；旧程序继续输出 Q0.0")
	res := rt.Reload(bad, DefaultConfig())
	if res.Applied || res.Reason != "ValidationRejected" {
		t.Fatalf("非法写目标重载必须拒绝: %+v", res)
	}
	if err := rt.Scan(); err != nil {
		t.Fatalf("旧程序必须继续运行: %v", err)
	}
	if !io.outBit(0, 0) {
		t.Fatal("拒绝重载后旧程序语义必须保持")
	}

	// 非法新配置（演练配置非法）也必须在同一条热重载口径下整体拒绝。
	badCfg := DefaultConfig()
	badCfg.ExecFaultScan = 1
	badCfg.ExecFaultAfterInstr = 99
	if res := rt.Reload(good, badCfg); res.Applied || res.Reason != "ValidationRejected" {
		t.Fatalf("非法配置热重载必须拒绝: %+v", res)
	}
}
