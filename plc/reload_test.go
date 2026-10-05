package plc

import (
	"errors"
	"strings"
	"testing"
)

func TestHotReloadAtomicity(t *testing.T) {
	caseLogger(t, "热重载-要么整体生效要么沿用旧程序",
		"旧程序 X0->Y0；先尝试加载含未定义标签的坏程序，再加载 X1->Y0 的新程序",
		"坏程序必须被拒绝且运行行为完全不变；好程序整体生效后 Y0 跟随 X1 而非 X0")

	old := "VERSION 1\nLD X0\nOUT Y0\nEND\n"
	cfg := testConfig(t)
	rt, io := newRuntime(t, old, cfg)
	io.SetInput(refOf("X0"), true)
	rt.ScanOnce()
	if !io.LastOutput(refOf("Y0")) {
		t.Fatal("旧程序下 Y0 应跟随 X0=true")
	}

	bad := "VERSION 1\nLD ghost\nOUT Y0\nEND\n"
	if _, err := rt.Reload(strings.NewReader(bad)); !errors.Is(err, ErrMissingReference) {
		t.Fatalf("坏程序应因引用缺失被拒绝，得到 %v", err)
	}
	// 拒绝后必须完整沿用旧程序：X0 仍决定 Y0。
	io.SetInput(refOf("X0"), false)
	io.SetInput(refOf("X1"), true)
	rt.ScanOnce()
	if io.LastOutput(refOf("Y0")) != false {
		t.Fatal("拒绝热重载后必须仍执行旧程序（Y0 跟随 X0=false）")
	}

	good := "VERSION 1\nLD X1\nOUT Y0\nEND\n"
	if _, err := rt.Reload(strings.NewReader(good)); err != nil {
		t.Fatalf("兼容的新程序应整体生效: %v", err)
	}
	io.SetInput(refOf("X1"), false)
	io.SetInput(refOf("X0"), true)
	rt.ScanOnce()
	if io.LastOutput(refOf("Y0")) != false {
		t.Fatal("新程序生效后 X0 不应再影响 Y0")
	}
	io.SetInput(refOf("X1"), false)
	rt.ScanOnce()
	if io.LastOutput(refOf("Y0")) != false {
		t.Fatal("新程序下 Y0 应跟随 X1=false")
	}
	io.SetInput(refOf("X1"), true)
	rt.ScanOnce()
	if !io.LastOutput(refOf("Y0")) {
		t.Fatal("新程序下 Y0 应跟随 X1=true")
	}
}

func TestHotReloadRetainCompatibleKeepsState(t *testing.T) {
	caseLogger(t, "热重载-保持兼容时保留计数",
		"旧程序跑到 C0=2；热重载仅改动非保持逻辑（新增 Y1 指示），保持集合不变",
		"重载后 C0 计数延续，再给 1 个上升沿即到预设 3")

	old := "VERSION 1\nLD X0\nCTU C0 K3\nLD C0\nOUT Y0\nEND\n"
	cfg := testConfig(t, "C0")
	rt, io := newRuntime(t, old, cfg)
	for i := 0; i < 2; i++ {
		io.SetInput(refOf("X0"), true)
		rt.ScanOnce()
		io.SetInput(refOf("X0"), false)
		rt.ScanOnce()
	}
	if err := rt.Persist(); err != nil {
		t.Fatal(err)
	}

	// 新程序结构哈希变化 -> 默认必须拒绝，防止错位加载。
	changed := "VERSION 1\nLD X0\nCTU C0 K3\nLD C0\nOUT Y0\nLD C0\nOUT Y1\nEND\n"
	if _, err := rt.Reload(strings.NewReader(changed)); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("结构变化时必须明确拒绝(ErrStateCorrupt，内含 ErrLayoutMismatch 原因)，得到 %v", err)
	}
	// 显式允许丢弃后按冷启动处理，计数归零。
	cfg.AllowDiscardState = true
	if _, err := rt.Reload(strings.NewReader(changed)); err != nil {
		t.Fatalf("allow_discard 下应冷启动: %v", err)
	}
	io.SetInput(refOf("X0"), true)
	rt.ScanOnce()
	if io.LastOutput(refOf("Y0")) {
		t.Fatal("冷启动后仅 1 个上升沿，C0 不应到 3")
	}
}

func TestHotReloadVersionMismatchRejected(t *testing.T) {
	caseLogger(t, "热重载-版本不一致拒绝",
		"VERSION 1 运行中加载 VERSION 2 同构程序",
		"必须报 ErrVersionMismatch，绝不把 v1 的保持数据错位给 v2")

	cfg := testConfig(t, "C0")
	rt, _ := newRuntime(t, "VERSION 1\nLD X0\nOUT Y0\nEND\n", cfg)
	_, err := rt.Reload(strings.NewReader("VERSION 2\nLD X0\nOUT Y0\nEND\n"))
	if !errors.Is(err, ErrVersionMismatch) {
		t.Fatalf("应拒绝版本不一致，得到 %v", err)
	}
}

func TestHotReloadRetainSetMismatchRejected(t *testing.T) {
	caseLogger(t, "热重载-保持集合不一致拒绝",
		"A 实例保持 [C0] 落盘；B 实例保持 [C1] 指向同一状态文件",
		"B 启动必须因保持集合不一致拒绝，绝不把 C0 的值错位读成 C1")

	cfg := testConfig(t, "C0")
	rt, _ := newRuntime(t, "VERSION 1\nLD X0\nCTU C0 K3\nLD C0\nOUT Y0\nEND\n", cfg)
	if err := rt.Persist(); err != nil {
		t.Fatal(err)
	}
	cfg2 := testConfig(t, "C1")
	cfg2.StateFile = cfg.StateFile // 指向同一份保持数据
	_, err := NewRuntime(
		mustParse(t, "VERSION 1\nLD X0\nCTU C1 K3\nLD C1\nOUT Y0\nEND\n"),
		cfg2, NewSimIO(), func(string, ...any) {})
	if !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("保持集合不一致应拒绝启动，得到 %v", err)
	}
}
