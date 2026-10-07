package plc

import (
	"strings"
	"testing"
)

// TestWriteTargetRejections 验证“目标不合法”在加载阶段按 IllegalWriteTarget 分类拒绝：
// 运行时自己维护的状态量（定时器/计数器元件及其字段）不能作为写入目标。
func TestWriteTargetRejections(t *testing.T) {
	cases := []struct {
		business string
		source   string
	}{
		{"OUT 目标是定时器输出位", "LD I0.0\nOUT T0.Q\nEND\n"},
		{"OUT 目标是计数器完成位", "LD I0.0\nOUT C0.Q\nEND\n"},
		{"SET 目标是定时器字段", "LD I0.0\nSET T0.Q\nEND\n"},
		{"SET 目标是计数器字段", "LD I0.0\nSET C0.Q\nEND\n"},
		{"MOV 目标是定时器预设", "MOV 100 T0.PT\nEND\n"},
		{"MOV 目标是定时器累计值", "MOV 0 T0.ET\nEND\n"},
		{"MOV 目标是计数器当前值", "MOV 3 C0.CV\nEND\n"},
		{"MOV 目标是计数器预设", "MOV 3 C0.PV\nEND\n"},
		{"RST 目标是定时器字段", "LD I0.0\nRST T0.ET\nEND\n"},
		{"RST 目标是定时器整体", "LD I0.0\nRST T0\nEND\n"},
		{"RST 目标是计数器字段", "LD I0.0\nRST C0.CV\nEND\n"},
	}
	for _, tc := range cases {
		t.Run(tc.business, func(t *testing.T) {
			caseLog(t, "写目标校验-"+tc.business, strings.ReplaceAll(tc.source, "\n", " | "),
				"Parse 返回 LoadError[IllegalWriteTarget]，加载阶段整体拒绝，不产生部分程序")
			_, err := Parse(tc.source)
			if err == nil {
				t.Fatal("预期拒绝，但加载成功")
			}
			le, ok := err.(*LoadError)
			if !ok {
				t.Fatalf("错误类型不是 *LoadError: %T", err)
			}
			if le.Reason != "IllegalWriteTarget" {
				t.Fatalf("原因分类错误: got %s want IllegalWriteTarget (%v)", le.Reason, le)
			}
		})
	}
}

// TestWriteTargetAccepted 验证合法写目标不被误伤：
// Q/M 位与字、整体计数器复位、TON/CTU 自身元件都照常通过。
func TestWriteTargetAccepted(t *testing.T) {
	cases := []struct {
		business string
		source   string
	}{
		{"OUT 写输出位", "LD I0.0\nOUT Q0.0\nEND\n"},
		{"SET 写存储位", "LD I0.0\nSET M0.0\nEND\n"},
		{"MOV 写存储字", "MOV 1 MW0\nEND\n"},
		{"MOV 写输出字", "MOV 1 QW0\nEND\n"},
		{"RST 整体计数器", "LD I0.0\nRST C0\nEND\n"},
		{"RST 存储字", "LD I0.0\nRST MW0\nEND\n"},
		{"TON 自身定时器元件", "LD I0.0\nTON T0 10\nEND\n"},
		{"CTU 自身计数器元件", "LD I0.0\nCTU C0 3 I0.0 I0.1\nEND\n"},
	}
	for _, tc := range cases {
		t.Run(tc.business, func(t *testing.T) {
			caseLog(t, "写目标校验-合法目标通过", strings.ReplaceAll(tc.source, "\n", " | "),
				"Parse 无错误，既有行为不回归")
			if _, err := Parse(tc.source); err != nil {
				t.Fatalf("合法写目标被误拒: %v", err)
			}
		})
	}
}

// TestWriteTargetColdStartAndHotReload 验证写目标校验在冷启动与热重载
// 两条路径口径一致：都是整体拒绝，热重载时旧程序原样继续跑。
func TestWriteTargetColdStartAndHotReload(t *testing.T) {
	badSrc := "LD I0.0\nOUT T0.Q\nEND\n"
	goodSrc := "LD I0.0\nOUT Q0.0\nEND\n"

	caseLog(t, "写目标校验-冷启动拒绝", "NewRuntime(OUT T0.Q)",
		"返回 LoadError[IllegalWriteTarget]，运行时不创建")
	_, err := NewRuntime(badSrc, DefaultConfig(), newFakeIO(1, 1))
	if err == nil {
		t.Fatal("冷启动必须拒绝非法写目标")
	}
	le, ok := err.(*LoadError)
	if !ok || le.Reason != "IllegalWriteTarget" {
		t.Fatalf("冷启动拒绝分类错误: %v", err)
	}

	caseLog(t, "写目标校验-热重载拒绝", "运行 goodSrc 中 Reload(OUT T0.Q)",
		"Reload Applied=false/ValidationRejected，旧程序原样继续驱动 Q0.0")
	rt, io := newTestRuntime(t, goodSrc, DefaultConfig())
	io.setBit(0, 0, true)
	if err := rt.Scan(); err != nil {
		t.Fatal(err)
	}
	res := rt.Reload(badSrc, DefaultConfig())
	if res.Applied || res.Reason != "ValidationRejected" {
		t.Fatalf("热重载必须整体拒绝: %+v", res)
	}
	if !strings.Contains(res.Detail, "IllegalWriteTarget") {
		t.Fatalf("拒绝详情应包含分类: %s", res.Detail)
	}
	if err := rt.Scan(); err != nil {
		t.Fatalf("旧程序应继续运行: %v", err)
	}
	if !io.outBit(0, 0) {
		t.Fatal("旧程序语义必须保持")
	}
}
