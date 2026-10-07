package plc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// FileConfig 是 JSON 配置文件结构。
type FileConfig struct {
	ScanPeriodMS         int64               `json:"scan_period_ms"`
	WatchdogInstructions int                 `json:"watchdog_instructions"`
	ScanTimeLimitMS      int64               `json:"scan_time_limit_ms"`
	FaultInjection       *FileFaultInjection `json:"fault_injection"`
	Retain               []string            `json:"retain"`
	RetainFile           string              `json:"retain_file"`
	SafeOutputs          map[string]int64    `json:"safe_outputs"`
}

// FileFaultInjection 是 JSON 中的本地故障演练配置；缺省（null）表示关闭。
type FileFaultInjection struct {
	ExecErrorScans []int64 `json:"exec_error_scans"`
	AtInstruction  int     `json:"at_instruction"`
}

// LoadConfigFile 从本地 JSON 读取配置；字段非法时由 NewRuntime 统一拒绝。
func LoadConfigFile(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("读取配置文件失败: %w", err)
	}
	var fc FileConfig
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fc); err != nil {
		return Config{}, fmt.Errorf("解析配置文件失败: %w", err)
	}
	cfg := DefaultConfig()
	if fc.ScanPeriodMS != 0 {
		cfg.ScanPeriodMS = fc.ScanPeriodMS
	}
	if fc.WatchdogInstructions != 0 {
		cfg.WatchdogInstructions = fc.WatchdogInstructions
	}
	cfg.ScanTimeLimitMS = fc.ScanTimeLimitMS
	if fc.FaultInjection != nil {
		cfg.FaultInjection = &FaultInjectionConfig{
			ExecErrorScans: append([]int64(nil), fc.FaultInjection.ExecErrorScans...),
			AtInstruction:  fc.FaultInjection.AtInstruction,
		}
	}
	cfg.Retain = fc.Retain
	cfg.RetainFile = fc.RetainFile
	if fc.SafeOutputs != nil {
		cfg.SafeOutputs = fc.SafeOutputs
	}
	return cfg, nil
}
