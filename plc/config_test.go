package plc

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestConfigValidation(t *testing.T) {
	caseLogger(t, "配置-非法项必须分别拒绝",
		"逐项构造非法周期/看门狗/预算/保持点/安全点配置",
		"Validate 必须返回 ErrInvalidConfig 且原因可解释；合法配置派生集合正确")

	base := func() *Config {
		return &Config{
			CycleIntervalMillis:   10,
			WatchdogMillis:        5,
			InstructionBudget:     100,
			IOSampleTimeoutMillis: 5,
			StateFile:             filepath.Join(t.TempDir(), "s.bin"),
		}
	}
	mkBad := func(mut func(*Config)) error {
		c := base()
		mut(c)
		return c.Validate()
	}

	if err := mkBad(func(c *Config) { c.CycleIntervalMillis = 0 }); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("周期非法: %v", err)
	}
	if err := mkBad(func(c *Config) { c.WatchdogMillis = -1 }); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("看门狗非法: %v", err)
	}
	if err := mkBad(func(c *Config) { c.InstructionBudget = 0 }); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("预算非法: %v", err)
	}
	if err := mkBad(func(c *Config) { c.IOSampleTimeoutMillis = 0 }); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("I/O 超时非法: %v", err)
	}
	if err := mkBad(func(c *Config) { c.StateFile = "" }); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("状态文件路径为空: %v", err)
	}
	if err := mkBad(func(c *Config) { c.SafeOutputs = map[string]bool{"X0": true} }); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("安全点不允许 X: %v", err)
	}
	if err := mkBad(func(c *Config) { c.Retain = []string{"X0"} }); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("X 不可保持: %v", err)
	}
	if err := mkBad(func(c *Config) { c.Retain = []string{"C0", "C0"} }); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("重复保持点: %v", err)
	}
	if err := mkBad(func(c *Config) { c.Retain = []string{"BAD"} }); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("非法保持地址: %v", err)
	}

	c := base()
	c.Retain = []string{"C1", "C0", "T0"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	keys := refsToKeys(c.RetainRefs())
	if keys[0] != "C0" || keys[1] != "C1" || keys[2] != "T0" {
		t.Fatalf("保持点应被排序，得到 %v", keys)
	}
}
