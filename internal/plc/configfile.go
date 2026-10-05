package plc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// FileConfig 是 JSON 配置文件结构。
type FileConfig struct {
	ScanPeriodMS         int64            `json:"scan_period_ms"`
	WatchdogInstructions int              `json:"watchdog_instructions"`
	Retain               []string         `json:"retain"`
	RetainFile           string           `json:"retain_file"`
	SafeOutputs          map[string]int64 `json:"safe_outputs"`
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
	cfg.Retain = fc.Retain
	cfg.RetainFile = fc.RetainFile
	if fc.SafeOutputs != nil {
		cfg.SafeOutputs = fc.SafeOutputs
	}
	return cfg, nil
}
