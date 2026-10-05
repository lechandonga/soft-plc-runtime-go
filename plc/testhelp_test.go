package plc

import (
	"path/filepath"
	"testing"
)

func testConfig(t *testing.T, retain ...string) *Config {
	t.Helper()
	c := &Config{
		CycleIntervalMillis:   10,
		WatchdogMillis:        200,
		InstructionBudget:     100000,
		IOSampleTimeoutMillis: 100,
		SafeOutputs:           map[string]bool{"Y0": true, "Y1": false},
		Retain:                retain,
		StateFile:             filepath.Join(t.TempDir(), "state.bin"),
		PersistEveryCycle:     false,
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("测试配置非法: %v", err)
	}
	return c
}

func mustParse(t *testing.T, src string) *Program {
	t.Helper()
	p, err := parseStr(src)
	if err != nil {
		t.Fatalf("测试程序解析失败: %v\n程序:\n%s", err, src)
	}
	return p
}

func refOf(s string) Ref {
	r, err := ParseAddress(s)
	if err != nil {
		panic(err)
	}
	return r
}

func newRuntime(t *testing.T, src string, cfg *Config) (*Runtime, *SimIO) {
	t.Helper()
	io := NewSimIO()
	rt, err := NewRuntime(mustParse(t, src), cfg, io, func(string, ...any) {})
	if err != nil {
		t.Fatalf("NewRuntime 失败: %v", err)
	}
	return rt, io
}
