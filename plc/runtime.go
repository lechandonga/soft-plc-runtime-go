package plc

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// FaultCode 是可区分的故障类别。
type FaultCode string

const (
	FaultWatchdog FaultCode = "WATCHDOG"
	FaultExec     FaultCode = "EXEC"
	FaultIO       FaultCode = "IO_LOST"
	FaultState    FaultCode = "STATE_WRITE"
)

// Fault 记录故障类别、发生周期与原因。
type Fault struct {
	Code   FaultCode
	Cycle  int64
	Reason string
}

func (f Fault) Error() string {
	return fmt.Sprintf("故障 %s @周期%d: %s", f.Code, f.Cycle, f.Reason)
}

// Unwrap 让 WATCHDOG/EXEC/IO_LOST/STATE_WRITE 可被 errors.Is
// 精确匹配到对应哨兵错误。
func (f Fault) Unwrap() error {
	switch f.Code {
	case FaultWatchdog:
		return ErrWatchdog
	case FaultExec:
		return ErrExecException
	case FaultIO:
		return ErrIOLost
	case FaultState:
		return ErrStateFault
	}
	return nil
}

// ExecHook 供测试确定性注入“执行异常”；每个周期执行前调用。
type ExecHook func(cycle int64) error

type timerData struct {
	done  bool
	accum int64
}

type counterData struct {
	done    bool
	current int64
	prev    bool
}

// Runtime 串行化扫描与管理操作：任何时刻只有一个周期在执行，
// 热重载与扫描互斥，杜绝新旧指令混跑。
type Runtime struct {
	mu sync.Mutex

	cfg *Config
	io  IOModule

	prog     *Program
	timers   map[Ref]*timerData
	counters map[Ref]*counterData
	words    map[Ref]int64
	bits     map[Ref]bool

	inputs   map[Ref]bool
	outputs  map[Ref]bool
	prevEdge map[Ref]bool

	cycle      int64
	lastScanMS int64
	fault      *Fault
	pending    *pendingWork

	store *StateStore
	hook  ExecHook

	stop chan struct{}
	logf func(string, ...any)
}

// NewRuntime 装配运行时并恢复保持数据。
func NewRuntime(prog *Program, cfg *Config, io IOModule, logf func(string, ...any)) (*Runtime, error) {
	if prog == nil || cfg == nil || io == nil {
		return nil, errors.New("prog/cfg/io 均不能为空")
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	store := NewStateStore(cfg.StateFile)
	res, err := store.Load(prog.Version, prog.StructureHash(), cfg.RetainRefs(), cfg.AllowDiscardState)
	if err != nil {
		return nil, err
	}
	logf("启动: 程序 v%d 结构 %s 保持点 %v —— %s",
		prog.Version, prog.StructureHash()[:12], refsToKeys(cfg.RetainRefs()), res.Reason)

	rt := &Runtime{
		cfg:      cfg,
		io:       io,
		prog:     prog,
		timers:   map[Ref]*timerData{},
		counters: map[Ref]*counterData{},
		words:    map[Ref]int64{},
		bits:     map[Ref]bool{},
		inputs:   map[Ref]bool{},
		outputs:  map[Ref]bool{},
		prevEdge: map[Ref]bool{},
		store:    store,
		logf:     logf,
	}
	rt.initState()
	if res.Present && res.State != nil {
		rt.restore(res.State)
	}
	return rt, nil
}

func (rt *Runtime) initState() {
	for _, r := range rt.cfg.RetainRefs() {
		switch r.Kind {
		case KindT:
			rt.timers[r] = &timerData{}
		case KindC:
			rt.counters[r] = &counterData{}
		case KindD:
			rt.words[r] = 0
		case KindM:
			rt.bits[r] = false
		}
	}
}

func (rt *Runtime) restore(snap *Snapshot) {
	for k, v := range snap.Bits {
		if r, err := ParseAddress(k); err == nil {
			rt.bits[r] = v
		}
	}
	for k, v := range snap.Words {
		if r, err := ParseAddress(k); err == nil {
			rt.words[r] = v
		}
	}
	for k, t := range snap.Timers {
		if r, err := ParseAddress(k); err == nil {
			rt.timers[r] = &timerData{done: t.Done, accum: t.Accum}
		}
	}
	for k, c := range snap.Counters {
		if r, err := ParseAddress(k); err == nil {
			rt.counters[r] = &counterData{done: c.Done, current: c.Current}
		}
	}
}

// SetExecHook 注入测试用执行异常钩子。
func (rt *Runtime) SetExecHook(h ExecHook) {
	rt.mu.Lock()
	rt.hook = h
	rt.mu.Unlock()
}

// Fault 返回当前故障快照；nil 表示正常。
func (rt *Runtime) Fault() *Fault {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.fault == nil {
		return nil
	}
	f := *rt.fault
	return &f
}

func (rt *Runtime) Cycle() int64 {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.cycle
}

// LastScanMillis 返回最近一个成功周期的执行耗时。
func (rt *Runtime) LastScanMillis() int64 {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.lastScanMS
}

// Output 返回最近提交的某个输出值。
func (rt *Runtime) Output(r Ref) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.outputs[r]
}

// Run 按配置周期驱动扫描，直到 Stop。
func (rt *Runtime) Run() {
	rt.mu.Lock()
	rt.stop = make(chan struct{})
	stop := rt.stop
	rt.mu.Unlock()

	ticker := time.NewTicker(time.Duration(rt.cfg.CycleIntervalMillis) * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			rt.ScanOnce()
		}
	}
}

func (rt *Runtime) Stop() {
	rt.mu.Lock()
	if rt.stop != nil {
		close(rt.stop)
		rt.stop = nil
	}
	rt.mu.Unlock()
}
