package plc

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// SnapshotForPersist 收集配置为保持的元件，形成完整快照。
func (rt *Runtime) buildSnapshot() *Snapshot {
	snap := &Snapshot{
		Manifest: Manifest{
			ProgramVersion: rt.prog.Version,
			StructureHash:  rt.prog.StructureHash(),
			Retain:         refsToKeys(rt.cfg.RetainRefs()),
		},
		Bits:     map[string]bool{},
		Words:    map[string]int64{},
		Timers:   map[string]TimerState{},
		Counters: map[string]CounterState{},
	}
	for _, r := range rt.cfg.RetainRefs() {
		switch r.Kind {
		case KindM:
			snap.Bits[r.String()] = rt.bits[r]
		case KindD:
			snap.Words[r.String()] = rt.words[r]
		case KindT:
			if t := rt.timers[r]; t != nil {
				snap.Timers[r.String()] = TimerState{Done: t.done, Accum: t.accum}
			}
		case KindC:
			if c := rt.counters[r]; c != nil {
				snap.Counters[r.String()] = CounterState{Done: c.done, Current: c.current}
			}
		}
	}
	return snap
}

func (rt *Runtime) persist() error {
	if err := rt.store.Save(rt.buildSnapshot()); err != nil {
		return fmt.Errorf("%w: 保持数据原子提交失败: %v", ErrStateFault, err)
	}
	return nil
}

// Persist 供停机前手动确认提交一次（锁内）。
func (rt *Runtime) Persist() error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.persist()
}

// Reset 是唯一的故障恢复入口，必须满足明确条件，拒绝悄悄自动恢复：
//   - WATCHDOG / EXEC：程序与配置未变即可由人工确认复位；
//   - IO_LOST：必须先确认 I/O 模块可成功完成一次读+写；
//   - STATE_WRITE：必须确认保持数据可成功完整写入。
func (rt *Runtime) Reset() error {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	if rt.fault == nil {
		return nil
	}
	code := rt.fault.Code

	switch code {
	case FaultIO:
		timeout := time.Duration(rt.cfg.IOSampleTimeoutMillis) * time.Millisecond
		if _, err := rt.io.ReadInputs(timeout); err != nil {
			return fmt.Errorf("%w: I/O 失联复位被拒绝，读取仍失败: %v", ErrIOLost, err)
		}
		probe := map[Ref]bool{}
		if err := rt.io.WriteOutputs(probe, timeout); err != nil {
			return fmt.Errorf("%w: I/O 失联复位被拒绝，写入仍失败: %v", ErrIOLost, err)
		}
	case FaultState:
		if err := rt.store.Save(rt.buildSnapshot()); err != nil {
			return fmt.Errorf("%w: 状态故障复位被拒绝，写入仍失败: %v", ErrStateFault, err)
		}
	case FaultWatchdog, FaultExec:
		// 人工确认即条件本身；不做任何自动尝试。
	default:
		return fmt.Errorf("未知故障类别 %s", code)
	}

	rt.logf("故障 %s 在周期 %d 之后经显式复位清除", code, rt.fault.Cycle)
	rt.fault = nil
	return nil
}

// Reload 原子热重载：新程序必须通过全部启动前校验，
// 且保持数据身份必须与当前运行实例对得上（或显式允许丢弃）。
// 任何一步失败都完整沿用旧程序继续运行，不出现新旧指令混跑。
func (rt *Runtime) Reload(r io.Reader) (*Program, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	old := rt.prog
	np, err := ParseProgram(r)
	if err != nil {
		rt.logf("热重载被拒绝，继续沿用旧程序: %v", err)
		return old, err
	}

	// 保持数据兼容性：版本、结构、保持集合三者必须一致，
	// 或配置显式 allow_discard_state=true 时冷启动非保持状态。
	res, err := rt.store.Load(np.Version, np.StructureHash(), rt.cfg.RetainRefs(), rt.cfg.AllowDiscardState)
	if err != nil {
		rt.logf("热重载被拒绝（保持数据不兼容），继续沿用旧程序: %v", err)
		return old, err
	}
	// 即便没有保持点，程序版本也不允许热重载时静默漂移：
	// VERSION 是显式的兼容契约，变化必须经 allow_discard_state 确认。
	if !res.Present {
		if np.Version != old.Version {
			err := fmt.Errorf("%w: 热重载程序版本 v%d -> v%d，需 allow_discard_state 确认",
				ErrVersionMismatch, old.Version, np.Version)
			rt.logf("热重载被拒绝，继续沿用旧程序: %v", err)
			return old, err
		}
	}

	rt.prog = np
	rt.timers = map[Ref]*timerData{}
	rt.counters = map[Ref]*counterData{}
	rt.words = map[Ref]int64{}
	rt.bits = map[Ref]bool{}
	rt.prevEdge = map[Ref]bool{}
	rt.initState()
	if res.Present && res.State != nil {
		rt.restore(res.State)
	}
	rt.cycle = 0
	rt.fault = nil
	rt.pending = nil
	// 新程序从确定的干净输出镜像起步；首个周期会重新计算并提交全部 Y 点。
	rt.outputs = map[Ref]bool{}
	rt.inputs = map[Ref]bool{}
	rt.logf("热重载整体生效: 新程序 v%d 结构 %s —— %s",
		np.Version, np.StructureHash()[:12], res.Reason)
	return np, nil
}

// LoadProgramFile 是 ParseProgram 的文件便捷封装。
func LoadProgramFile(path string) (*Program, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ParseProgram(f)
}

// IsStateCorrupt 便于上层判断是否属于保持数据兼容问题。
func IsStateCorrupt(err error) bool { return errors.Is(err, ErrStateCorrupt) }
