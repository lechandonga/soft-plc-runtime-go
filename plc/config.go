package plc

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// Config 是运行时的全部可配置项。零值必须配合 Validate 使用，不做静默默认顶替。
type Config struct {
	// CycleIntervalMillis 为扫描周期目标长度（毫秒）。
	CycleIntervalMillis int `json:"cycle_interval_ms"`
	// WatchdogMillis 是单个周期允许的最长执行时间；超过即 WatchdogFault。
	WatchdogMillis int `json:"watchdog_ms"`
	// InstructionBudget 是单周期指令执行条数上限（测试/硬实时约束用）。
	InstructionBudget int64 `json:"instruction_budget"`
	// IOSampleTimeoutMillis 为单次输入采样/输出提交的超时。
	IOSampleTimeoutMillis int `json:"io_sample_timeout_ms"`
	// SafeOutputs 指定故障时各输出点的安全值；未列出的点安全值为 false。
	SafeOutputs map[string]bool `json:"safe_outputs"`
	// Retain 列出需要掉电保持的地址（T/C/D 及其状态）。
	Retain []string `json:"retain"`
	// StateFile 是保持数据文件路径。
	StateFile string `json:"state_file"`
	// PersistEveryCycle 为 true 时每个周期末尾原子提交一次保持数据。
	PersistEveryCycle bool `json:"persist_every_cycle"`
	// AllowDiscardState 为 true 时允许丢弃无法兼容的旧保持数据（冷启动）。
	AllowDiscardState bool `json:"allow_discard_state"`

	safeRefs map[Ref]bool
	retain   []Ref
}

// LoadConfig 读取并严格校验配置；任何非法项都拒绝启动。
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) Validate() error {
	if c.CycleIntervalMillis <= 0 {
		return fmt.Errorf("%w: cycle_interval_ms 必须为正整数", ErrInvalidConfig)
	}
	if c.WatchdogMillis <= 0 {
		return fmt.Errorf("%w: watchdog_ms 必须为正整数", ErrInvalidConfig)
	}
	if c.InstructionBudget <= 0 {
		return fmt.Errorf("%w: instruction_budget 必须为正整数", ErrInvalidConfig)
	}
	if c.IOSampleTimeoutMillis <= 0 {
		return fmt.Errorf("%w: io_sample_timeout_ms 必须为正整数", ErrInvalidConfig)
	}
	if c.StateFile == "" {
		return fmt.Errorf("%w: state_file 不能为空", ErrInvalidConfig)
	}
	c.safeRefs = map[Ref]bool{}
	c.retain = nil
	for addr, v := range c.SafeOutputs {
		r, err := ParseAddress(addr)
		if err != nil {
			return fmt.Errorf("%w: safe_outputs 中的 %q: %v", ErrInvalidConfig, addr, err)
		}
		if r.Kind != KindY {
			return fmt.Errorf("%w: safe_outputs 只能配置 Y 点，得到 %s", ErrInvalidConfig, addr)
		}
		c.safeRefs[r] = v
	}
	seen := map[Ref]bool{}
	for _, addr := range c.Retain {
		r, err := ParseAddress(addr)
		if err != nil {
			return fmt.Errorf("%w: retain 中的 %q: %v", ErrInvalidConfig, addr, err)
		}
		if r.Kind != KindT && r.Kind != KindC && r.Kind != KindD {
			return fmt.Errorf("%w: 只有 T/C/D 元件可配置保持，得到 %s", ErrInvalidConfig, addr)
		}
		if seen[r] {
			return fmt.Errorf("%w: retain 中 %s 重复", ErrInvalidConfig, addr)
		}
		seen[r] = true
		c.retain = append(c.retain, r)
	}
	sort.Slice(c.retain, func(i, j int) bool {
		if c.retain[i].Kind != c.retain[j].Kind {
			return c.retain[i].Kind < c.retain[j].Kind
		}
		return c.retain[i].Idx < c.retain[j].Idx
	})
	return nil
}

// RetainRefs 返回排序去重后的保持地址。
func (c *Config) RetainRefs() []Ref { return c.retain }

func (c *Config) safeValue(r Ref) bool {
	if c.safeRefs != nil {
		return c.safeRefs[r]
	}
	// 兼容未经过 Validate 重建的手工 Config（JSON 标签形式）。
	for addr, v := range c.SafeOutputs {
		if pr, err := ParseAddress(addr); err == nil && pr == r {
			return v
		}
	}
	return false
}
