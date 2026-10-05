package plc

import (
	"errors"
	"os"
	"testing"
)

const counterProgram = "VERSION 1\n" +
	"LD X0\nCTU C0 K5\n" +
	"LD C0\nOUT Y0\n" +
	"LD X0\nTON T0 K100\n" +
	"LD X1\nRST C0\n" +
	"END\n"

func TestRetainRestartConsistency(t *testing.T) {
	caseLogger(t, "保持-异常退出后恢复最近一次完整提交",
		"程序保持 C0/T0；跑 3 个计数上升沿并显式 Persist 后模拟进程退出，重建 Runtime",
		"新实例 C0 当前值必须恢复为 3，再给 2 个上升沿即到 5；未配置保持的 Y 按冷启动")

	cfg := testConfig(t, "C0", "T0")
	var statePath string
	{
		rt, io := newRuntime(t, counterProgram, cfg)
		statePath = cfg.StateFile
		for i := 0; i < 3; i++ {
			io.SetInput(refOf("X0"), true)
			rt.ScanOnce()
			io.SetInput(refOf("X0"), false)
			rt.ScanOnce()
		}
		if rt.counters[refOf("C0")].current != 3 {
			t.Fatalf("C0 应为 3，得到 %d", rt.counters[refOf("C0")].current)
		}
		if err := rt.Persist(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(statePath); err != nil {
			t.Fatalf("状态文件应存在: %v", err)
		}
		// 模拟进程退出：runtime 直接丢弃，不做任何额外清理。
	}

	cfg2 := testConfig(t, "C0", "T0")
	cfg2.StateFile = statePath
	rt2, io2 := newRuntimeWithLog(t, counterProgram, cfg2)
	if got := rt2.counters[refOf("C0")].current; got != 3 {
		t.Fatalf("重启后 C0 应恢复为 3，得到 %d", got)
	}
	// 再给 2 个上升沿（X1 保持 false），应在第 2 个后到点。
	for i := 0; i < 2; i++ {
		io2.SetInput(refOf("X0"), true)
		rt2.ScanOnce()
		io2.SetInput(refOf("X0"), false)
		rt2.ScanOnce()
	}
	if !io2.LastOutput(refOf("Y0")) {
		t.Fatal("恢复后再计 2 次应到达预设 5，C0 完成")
	}
}

func TestUncommittedCycleLostOnCrash(t *testing.T) {
	caseLogger(t, "保持-未提交周期不得生效",
		"Persist 后再跑 2 个计数上升沿但不 Persist，直接重建实例",
		"重启后 C0 必须仍是提交时的值 3，第 4/5 个未提交计数丢失（确定语义）")

	cfg := testConfig(t, "C0")
	rt, io := newRuntime(t, counterProgram, cfg)
	for i := 0; i < 3; i++ {
		io.SetInput(refOf("X0"), true)
		rt.ScanOnce()
		io.SetInput(refOf("X0"), false)
		rt.ScanOnce()
	}
	if err := rt.Persist(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		io.SetInput(refOf("X0"), true)
		rt.ScanOnce()
		io.SetInput(refOf("X0"), false)
		rt.ScanOnce()
	}

	cfg2 := testConfig(t, "C0")
	cfg2.StateFile = cfg.StateFile
	rt2, _ := newRuntimeWithLog(t, counterProgram, cfg2)
	if got := rt2.counters[refOf("C0")].current; got != 3 {
		t.Fatalf("未提交周期必须丢失，C0 应为 3，得到 %d", got)
	}
}

func TestTornStateFileRejected(t *testing.T) {
	caseLogger(t, "保持-写一半的文件不得生效",
		"手工构造：合法文件截断 / 翻转字节破坏校验和",
		"两种损坏都必须以 ErrStateCorrupt 拒绝；allow_discard=true 时明确冷启动")

	cfg := testConfig(t, "C0")
	rt, io := newRuntime(t, counterProgram, cfg)
	io.SetInput(refOf("X0"), true)
	rt.ScanOnce()
	if err := rt.Persist(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	truncated := append([]byte{}, raw[:len(raw)-40]...)
	if err := os.WriteFile(cfg.StateFile, truncated, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg2 := testConfig(t, "C0")
	cfg2.StateFile = cfg.StateFile
	if _, err := NewRuntime(mustParseT(t, counterProgram), cfg2, NewSimIO(), nil); err == nil {
		t.Fatal("截断文件必须被拒绝")
	}

	// 重建合法文件再破坏校验和。
	if err := rt.Persist(); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(cfg.StateFile)
	raw[len(raw)-1] ^= 0xFF
	if err := os.WriteFile(cfg.StateFile, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = NewRuntime(mustParseT(t, counterProgram), cfg2, NewSimIO(), nil)
	if !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("校验和损坏必须报 ErrStateCorrupt，得到 %v", err)
	}

	// 显式允许丢弃时冷启动，结论必须出现在错误/日志通道之外的正常启动路径。
	cfg3 := testConfig(t, "C0")
	cfg3.StateFile = cfg.StateFile
	cfg3.AllowDiscardState = true
	rt3, _ := newRuntimeWithLog(t, counterProgram, cfg3)
	if got := rt3.counters[refOf("C0")].current; got != 0 {
		t.Fatalf("丢弃损坏数据后必须冷启动，C0=0，得到 %d", got)
	}
}

func TestStateCompatibilityMatrix(t *testing.T) {
	caseLogger(t, "保持-程序身份兼容结论矩阵",
		"版本变化 / 结构变化 / 保持集合变化 / 完全一致 四种情形",
		"前三者分别得到明确拒绝原因；完全一致正常恢复")

	cfg := testConfig(t, "C0")
	rt, io := newRuntime(t, counterProgram, cfg)
	io.SetInput(refOf("X0"), true)
	rt.ScanOnce()
	io.SetInput(refOf("X0"), false)
	rt.ScanOnce()
	if err := rt.Persist(); err != nil {
		t.Fatal(err)
	}

	type tc struct {
		name    string
		program string
		retain  []string
		wantErr error
	}
	cases := []tc{
		{"完全一致", counterProgram, []string{"C0"}, nil},
		{"版本变化", "VERSION 2\n" + counterProgram[len("VERSION 1\n"):], []string{"C0"}, ErrVersionMismatch},
		{"结构变化", "VERSION 1\nLD X0\nCTU C0 K5\nLD C0\nOUT Y0\nEND\n", []string{"C0"}, ErrLayoutMismatch},
		{"保持集合变化", counterProgram, []string{"C0", "T0"}, ErrRetainMismatch},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cc := testConfig(t, c.retain...)
			cc.StateFile = cfg.StateFile
			_, err := NewRuntime(mustParseT(t, c.program), cc, NewSimIO(), func(string, ...any) {})
			if c.wantErr == nil {
				if err != nil {
					t.Fatalf("应兼容启动，得到 %v", err)
				}
				return
			}
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("want %v, got %v", c.wantErr, err)
			}
		})
	}
}

func mustParseT(t *testing.T, src string) *Program {
	t.Helper()
	return mustParse(t, src)
}

func newRuntimeWithLog(t *testing.T, src string, cfg *Config) (*Runtime, *SimIO) {
	t.Helper()
	io := NewSimIO()
	rt, err := NewRuntime(mustParse(t, src), cfg, io, func(format string, a ...any) {
		t.Logf("[启动日志] "+format, a...)
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	return rt, io
}
