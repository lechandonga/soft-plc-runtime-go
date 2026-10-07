package plc

// Config 是运行时的全部可配置项（纯本地、无外部依赖）。
type Config struct {
	// ScanPeriodMS 是扫描周期标称时长，也是定时器的确定时间基（毫秒）。
	ScanPeriodMS int64

	// WatchdogInstructions 是单周期指令条数预算，超限即 WatchdogFault。
	WatchdogInstructions int

	// ScanTimeLimitMS 是单周期墙钟耗时上限（毫秒）：从输入采样开始到输出提交
	// 完成为止，总耗时超过该值即进入 ScanTimeoutFault。
	// 0 表示关闭这道保护（默认），此时行为与引入该配置前完全一致。
	ScanTimeLimitMS int64

	// FaultInjection 是本地故障演练注入配置；nil 表示关闭（默认）。
	// 仅用于本地演练，开启后行为确定、可复现；关闭时对正常运行零影响。
	FaultInjection *FaultInjectionConfig

	// Retain 为保持项地址名称列表（如 "M0.0"、"MW2"、"C0"）；空表示不保持。
	Retain []string

	// RetainFile 是保持数据文件路径；空表示禁用掉电保持。
	RetainFile string

	// SafeOutputs 是故障时输出过程映像强制落定的安全值（地址名称 -> 0/1 或整数字节）。
	// 未列出的输出一律清 0。每次进入故障状态都会重新整包落定。
	SafeOutputs map[string]int64
}

// FaultInjectionConfig 描述“在某个扫描周期让执行阶段确定性报一次运行期异常”。
type FaultInjectionConfig struct {
	// ExecErrorScans 是注入异常的扫描周期号列表（1 起计，按运行时的扫描计数）。
	// 每个周期号在运行期生命周期内至多触发一次：故障周期不计数，
	// 显式恢复后重跑同一周期号时不再重复注入。
	ExecErrorScans []int64
	// AtInstruction 是注入点的指令序号（0 起）：执行到该指令前抛出异常，
	// 之前的半截写入随周期整体回滚，之后的指令本周期不再执行。
	AtInstruction int
}

// DefaultConfig 返回确定性默认配置：10ms 周期、100000 条指令预算、
// 墙钟上限与故障演练均关闭。
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
	if c.ScanTimeLimitMS < 0 {
		return c, loadErr("StructureError", 0, "ScanTimeLimitMS 不能为负（0 表示关闭）")
	}
	if c.WatchdogInstructions <= 0 {
		c.WatchdogInstructions = 100000
	}
	if fi := c.FaultInjection; fi != nil {
		if fi.AtInstruction < 0 {
			return c, loadErr("StructureError", 0, "FaultInjection.AtInstruction 不能为负")
		}
		for _, s := range fi.ExecErrorScans {
			if s <= 0 {
				return c, loadErr("StructureError", 0, "FaultInjection.ExecErrorScans 周期号必须从 1 起计")
			}
		}
	}
	if c.SafeOutputs == nil {
		c.SafeOutputs = map[string]int64{}
	}
	return c, nil
}

// validateFaultInjection 在程序已知后校验注入点落在程序范围内（冷启动与热重载共用）。
func validateFaultInjection(p *Program, fi *FaultInjectionConfig) error {
	if fi == nil {
		return nil
	}
	if fi.AtInstruction >= len(p.Statements) {
		return loadErr("StructureError", 0,
			"FaultInjection.AtInstruction=%d 超出程序指令数 %d", fi.AtInstruction, len(p.Statements))
	}
	return nil
}
