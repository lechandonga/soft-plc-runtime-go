package plc

// Config 是运行时的全部可配置项（纯本地、无外部依赖）。
type Config struct {
	// ScanPeriodMS 是扫描周期标称时长，也是定时器的确定时间基（毫秒）。
	ScanPeriodMS int64

	// WatchdogInstructions 是单周期指令条数预算，超限即 WatchdogFault。
	WatchdogInstructions int

	// Retain 为保持项地址名称列表（如 "M0.0"、"MW2"、"C0"）；空表示不保持。
	Retain []string

	// RetainFile 是保持数据文件路径；空表示禁用掉电保持。
	RetainFile string

	// SafeOutputs 是故障时输出过程映像强制落定的安全值（地址名称 -> 0/1 或整数字节）。
	// 未列出的输出一律清 0。每次进入故障状态都会重新整包落定。
	SafeOutputs map[string]int64
}

// DefaultConfig 返回确定性默认配置：10ms 周期、100000 条指令预算。
func DefaultConfig() Config {
	return Config{
		ScanPeriodMS:         10,
		WatchdogInstructions: 100000,
		SafeOutputs:          map[string]int64{},
	}
}

func (c Config) validated() (Config, error) {
	if c.ScanPeriodMS <= 0 {
		return c, loadErr("StructureError", 0, "ScanPeriodMS 必须为正")
	}
	if c.WatchdogInstructions <= 0 {
		c.WatchdogInstructions = 100000
	}
	if c.SafeOutputs == nil {
		c.SafeOutputs = map[string]int64{}
	}
	return c, nil
}
