package plc

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// FaultKind 区分故障来源，便于按原因处置。
type FaultKind string

const (
	FaultNone      FaultKind = ""
	FaultIO        FaultKind = "IOFault"        // I/O 失联（采样或提交）
	FaultWatchdog  FaultKind = "WatchdogFault"  // 单周期超时
	FaultExecution FaultKind = "ExecutionFault" // 程序执行异常
	FaultRetain    FaultKind = "RetainFault"    // 保持数据提交失败
	FaultStart     FaultKind = "StartupFault"   // 启动时保持数据不兼容
)

// Fault 记录当前故障状态与原因；不允许悄悄自动恢复。
type Fault struct {
	Kind    FaultKind
	Detail  string
	ScansAt int64
}

func (f *Fault) Error() string {
	return fmt.Sprintf("%s@scan#%d: %s", f.Kind, f.ScansAt, f.Detail)
}

func (f Fault) String() string {
	return fmt.Sprintf("%s@scan#%d: %s", f.Kind, f.ScansAt, f.Detail)
}

// IO 是运行时依赖的本地 I/O 抽象（由 simio 等实现）。
type IO interface {
	Read(size int) ([]byte, error)
	Write(out []byte) error
	Healthy() bool
}

// Runtime 执行严格的三阶段循环扫描，并管理故障与保持数据。
// 所有对外方法均互斥；Scan 与 Reload 并发调用也只会串行化，
// 绝不会出现“新旧指令混着执行”或“半更新输入”。
type Runtime struct {
	mu sync.Mutex

	prog   *Program
	cfg    Config
	layout RetainLayout
	io     IO
	store  *RetainStore

	data      *DataTable // 当前过程映像（故障时为安全输出态）
	committed *DataTable // 最近一次完整成功扫描结束时的快照（含全部状态）

	scanCount uint64
	fault     *Fault
	stopCh    chan struct{}
	stopped   bool

	// scanAttempts 是进入扫描流程的尝试序号（从 1 起，故障周期也计数），
	// 供执行故障演练按“第 N 个周期”确定性命中；跨恢复/重载单调递增。
	scanAttempts uint64
	// drillFired 保证演练在整个配置代内只触发一次；Reload 换新配置后重新装弹。
	drillFired bool

	// now 返回墙钟；为 nil 时使用 time.Now。测试可注入确定性虚拟时钟。
	now func() time.Time

	// Trace 在每个扫描阶段成功结束后回调（测试与日志用），持锁调用。
	Trace func(scan uint64, phase string)
}

// NewRuntime 完成程序启动前校验、保持数据恢复与安全输出初始化。
// 若保持文件与程序/布局不兼容，返回 *RetainMismatchError，拒绝启动（不做错位加载）。
func NewRuntime(source string, cfg Config, ioModule IO) (*Runtime, error) {
	cfg, err := cfg.validated()
	if err != nil {
		return nil, err
	}
	prog, err := Parse(source)
	if err != nil {
		return nil, err
	}
	layout, err := prog.BuildRetainLayout(cfg.Retain)
	if err != nil {
		return nil, err
	}
	if err := validateSafeOutputs(prog, cfg.SafeOutputs); err != nil {
		return nil, err
	}
	if err := validateDrill(prog, cfg); err != nil {
		return nil, err
	}
	r := &Runtime{
		prog: prog, cfg: cfg, layout: layout, io: ioModule,
		data:      newDataTable(prog),
		committed: newDataTable(prog),
		stopCh:    make(chan struct{}),
	}
	if cfg.RetainFile != "" {
		r.store = NewRetainStore(cfg.RetainFile)
		rec, lerr := r.store.Load(prog.Hash, layout.Hash())
		if lerr != nil {
			if errors.Is(lerr, os.ErrNotExist) {
				// 冷启动：无保持文件合法。
			} else {
				return nil, lerr
			}
		} else {
			if err := Restore(r.data, layout, rec); err != nil {
				return nil, err
			}
			r.committed = r.data.Clone()
		}
	}
	return r, nil
}

// validateDrill 校验执行故障演练参数：关闭时无任何约束；
// 开启时注入位置必须落在本周期确实能执行到的指令边界上，防止演练配置静默失效。
func validateDrill(p *Program, cfg Config) error {
	if cfg.ExecFaultScan == 0 {
		return nil
	}
	if cfg.ExecFaultAfterInstr >= len(p.Statements) {
		return loadErr("StructureError", 0,
			"ExecFaultAfterInstr=%d 超出程序指令边界数 %d，演练异常将永远无法触发",
			cfg.ExecFaultAfterInstr, len(p.Statements))
	}
	return nil
}

func validateSafeOutputs(p *Program, safe map[string]int64) error {
	for name := range safe {
		a, err := ParseOperand(name)
		if err != nil {
			return loadErr("IllegalAddress", 0, "安全输出 %q 非法: %v", name, err)
		}
		if a.Kind != OpOutput {
			return loadErr("IllegalAddress", 0, "安全输出 %q 必须是 Q 区地址", name)
		}
		var end int
		if a.Type == TypeBit {
			end = a.Offset/8 + 1
		} else {
			end = a.Offset/8 + a.Size/8
		}
		if end > p.OutputSize {
			return loadErr("ReferenceError", 0, "安全输出 %q 在程序中从未被引用", name)
		}
	}
	return nil
}

// ScanCount 返回已完整提交的扫描周期数（故障周期不计）。
func (r *Runtime) ScanCount() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.scanCount
}

// Fault 返回当前故障的副本；Kind 为 FaultNone 表示运行正常。
func (r *Runtime) Fault() Fault {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fault == nil {
		return Fault{}
	}
	return *r.fault
}

// SnapshotOutputs 返回当前输出映像副本（故障时为安全值）。
func (r *Runtime) SnapshotOutputs() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.data.Out...)
}

// SnapshotMemory 返回 M 区副本（测试判定用）。
func (r *Runtime) SnapshotMemory() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.data.Mem...)
}

// TimerState / CounterState 只读访问（测试与日志用）。
func (r *Runtime) TimerState(idx int) (TimerState, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if idx < 0 || idx >= len(r.data.Timers) {
		return TimerState{}, false
	}
	return r.data.Timers[idx], true
}

func (r *Runtime) CounterState(idx int) (CounterState, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if idx < 0 || idx >= len(r.data.Counters) {
		return CounterState{}, false
	}
	return r.data.Counters[idx], true
}
