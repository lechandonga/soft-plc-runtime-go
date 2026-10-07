package plc

// Config 是运行时的全部可配置项（纯本地、无外部依赖）。
type Config struct {
	// ScanPeriodMS 是扫描周期标称时长，也是定时器的确定时间基（毫秒）。
	ScanPeriodMS int64

	// WatchdogInstructions 是单周期指令条数预算，超限即 WatchdogFault。
	WatchdogInstructions int

	// WatchdogTimeMS 是单周期墙钟上限（毫秒），从本周期输入采样前起计时，
	// 到输出提交完成为止；总耗时超过即 WatchdogFault（原因记为墙钟到点）。
	// 0 表示关闭该道保护（缺省即关闭，老配置行为完全不变）；不允许负值。
	// 与指令条数预算互相独立：同一条指令边界上先判墙钟，再判条数。
	WatchdogTimeMS int64

	// ExecFaultScan / ExecFaultScanEnd 是“执行故障演练”窗口（按扫描尝试序号，从 1 起）。
	// 两者均为 0 表示演练关闭（缺省），对正常运行没有任何影响。
	// ExecFaultScanEnd 为 0 表示只在 ExecFaultScan 指定的单个周期触发；
	// 否则在闭区间 [ExecFaultScan, ExecFaultScanEnd] 的第一个到达周期触发，
	// 整个运行期只确定性地触发一次（热重载后重新装弹）。
	ExecFaultScan    int64
	ExecFaultScanEnd int64

	// ExecFaultAfterInstr 指定演练异常在该周期第几条指令执行完之后抛出
	// （0 表示第一条指令执行前；用于验证异常点之前半截写入的整体回滚）。
	ExecFaultAfterInstr int

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
		// WatchdogTimeMS 缺省 0：墙钟保护关闭，既有配置/演示/用例行为逐拍不变。
		SafeOutputs: map[string]int64{},
	}
}

func (c Config) validated() (Config, error) {
	if c.ScanPeriodMS <= 0 {
		return c, loadErr("StructureError", 0, "ScanPeriodMS 必须为正")
	}
	if c.WatchdogInstructions <= 0 {
		c.WatchdogInstructions = 100000
	}
	if c.WatchdogTimeMS < 0 {
		return c, loadErr("StructureError", 0, "WatchdogTimeMS 不能为负（0 表示关闭墙钟保护）")
	}
	if c.ExecFaultScan < 0 || c.ExecFaultScanEnd < 0 || c.ExecFaultAfterInstr < 0 {
		return c, loadErr("StructureError", 0, "执行故障演练参数不能为负")
	}
	if c.ExecFaultScanEnd != 0 && c.ExecFaultScanEnd < c.ExecFaultScan {
		return c, loadErr("StructureError", 0, "ExecFaultScanEnd 不能小于 ExecFaultScan")
	}
	if c.ExecFaultScan == 0 && (c.ExecFaultScanEnd != 0 || c.ExecFaultAfterInstr != 0) {
		return c, loadErr("StructureError", 0, "执行故障演练必须先指定 ExecFaultScan 才能开启")
	}
	if c.SafeOutputs == nil {
		c.SafeOutputs = map[string]int64{}
	}
	return c, nil
}

// drillActiveAt 报告演练是否应在第 ord 个扫描尝试（从 1 起）注入异常。
func (c Config) drillActiveAt(ord int64, alreadyFired bool) bool {
	if alreadyFired || c.ExecFaultScan == 0 {
		return false
	}
	end := c.ExecFaultScanEnd
	if end == 0 {
		end = c.ExecFaultScan
	}
	return ord >= c.ExecFaultScan && ord <= end
}
