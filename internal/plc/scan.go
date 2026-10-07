package plc

import (
	"errors"
	"fmt"
	"time"
)

// Scan 执行一次严格的“输入采样 -> 程序执行 -> 输出提交 -> 保持提交”周期。
// 任何阶段失败都会：回滚到周期前已提交状态、整包落定安全输出、进入对应故障状态，
// 且不自动恢复——必须显式调用 Recover，且满足 I/O 健康等条件。
func (r *Runtime) Scan() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.stopped {
		return errors.New("运行时已停止")
	}
	if r.fault != nil {
		// 故障锁存：不悄悄自动恢复，也不推进扫描。
		return r.fault
	}

	// 周期前完整快照：任何阶段失败都整体回滚，杜绝“半生效”。
	preCycle := r.data.Clone()

	// 墙钟保护从此刻起算：输入采样前开始，覆盖采样+执行+输出提交全过程。
	now := r.now
	if now == nil {
		now = time.Now
	}
	cycleStart := now()

	// 墙钟截止检查（采样/提交之后各一道）；超时与条数预算互相独立。
	checkDeadline := func(lineRef int) error {
		if r.cfg.WatchdogTimeMS <= 0 {
			return nil
		}
		elapsed := now().Sub(cycleStart)
		if elapsed >= time.Duration(r.cfg.WatchdogTimeMS)*time.Millisecond {
			return &DeadlineExceededError{
				LineNo:    lineRef,
				LimitMS:   r.cfg.WatchdogTimeMS,
				ElapsedMS: int64(elapsed / time.Millisecond),
			}
		}
		return nil
	}

	// 阶段1：输入采样（整包快照；程序整个周期只看到这一份）。
	in, err := r.io.Read(r.prog.InputSize)
	if err != nil {
		return r.enterFault(preCycle, FaultIO, err.Error())
	}
	if err := checkDeadline(0); err != nil {
		return r.enterFault(preCycle, FaultWatchdog, err.Error())
	}
	copy(r.data.In, in)
	if r.Trace != nil {
		r.Trace(r.scanAttempts+1, "sample")
	}

	r.scanAttempts++

	// 阶段2：程序执行（工作副本上直接跑；失败整体回滚，工作状态不保留）。
	execCfg := ExecConfig{
		MaxInstructions: r.cfg.WatchdogInstructions,
		DeadlineMS:      r.cfg.WatchdogTimeMS,
		start:           cycleStart,
		now:             now,
	}
	if r.cfg.drillActiveAt(int64(r.scanAttempts), r.drillFired) {
		execCfg.injectFault = true
		execCfg.InjectFaultAfterInstr = r.cfg.ExecFaultAfterInstr
	}
	if err := Execute(r.prog, r.data, r.cfg.ScanPeriodMS, execCfg); err != nil {
		kind := FaultExecution
		detail := err.Error()
		var drilled *DrilledExecError
		if errors.As(err, &drilled) {
			drilled.ScanOrd = int64(r.scanAttempts)
			r.drillFired = true
			detail = drilled.Error()
		}
		var wd *WatchdogError
		var dl *DeadlineExceededError
		if errors.As(err, &wd) || errors.As(err, &dl) {
			kind = FaultWatchdog
		}
		return r.enterFault(preCycle, kind, detail)
	}
	r.drillFired = r.drillFired || execCfg.injectFault
	if r.Trace != nil {
		r.Trace(r.scanAttempts, "execute")
	}

	// 阶段3：输出提交（整包写入，外部只看到完整新包）。
	if err := r.io.Write(r.data.Out); err != nil {
		return r.enterFault(preCycle, FaultIO, err.Error())
	}
	if err := checkDeadline(0); err != nil {
		return r.enterFault(preCycle, FaultWatchdog, err.Error())
	}
	if r.Trace != nil {
		r.Trace(r.scanAttempts, "commit")
	}

	// 阶段4：保持数据提交（先扫描后持久化；失败进入 RetainFault，
	// 但本周期过程映像已对外提交，因此保留现场并落安全值后报警）。
	r.scanCount++
	r.committed = r.data.Clone()
	if r.store != nil {
		rec, err := Snapshot(r.data, r.prog.Hash, r.layout.Hash(), r.layout, r.scanCount)
		if err != nil {
			return r.enterFault(preCycle, FaultRetain, err.Error())
		}
		if err := r.store.Commit(rec); err != nil {
			return r.enterFault(preCycle, FaultRetain, err.Error())
		}
		if r.Trace != nil {
			r.Trace(r.scanCount, "retain")
		}
	}
	return nil
}

func (r *Runtime) enterFault(preCycle *DataTable, kind FaultKind, detail string) error {
	r.data = preCycle
	r.applySafeOutputs()
	// 向 I/O 整包落定安全值；若 I/O 本身已失联，失败仅记录不改变故障分类优先级。
	if writeErr := r.io.Write(r.data.Out); writeErr != nil && kind != FaultIO {
		kind = FaultIO
		detail = fmt.Sprintf("%s（且安全输出无法下发: %v）", detail, writeErr)
	}
	r.fault = &Fault{Kind: kind, Detail: detail, ScansAt: int64(r.scanCount + 1)}
	return r.fault
}

func (r *Runtime) applySafeOutputs() {
	for i := range r.data.Out {
		r.data.Out[i] = 0
	}
	for name, val := range r.cfg.SafeOutputs {
		a, err := ParseOperand(name)
		if err != nil {
			continue // 启动期已校验
		}
		if a.Type == TypeBit {
			_ = r.data.writeBit(a, val != 0)
		} else {
			_ = r.data.writeWord(a, val)
		}
	}
}

// Recover 在满足明确条件后清除故障：
//  1. 必须存在已锁存的故障（正常运行调用返回错误，避免被当作复位手段滥用）；
//  2. I/O 模块必须 Healthy，且一次试读+安全输出试写必须成功；
//  3. RetainFault 还要求保持存储可写（试提交一次当前保持快照）。
//
// 恢复后从“最近一次完整提交的状态”继续，故障周期的半成品状态一律不存在。
func (r *Runtime) Recover() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fault == nil {
		return errors.New("当前无故障，无需恢复")
	}
	if !r.io.Healthy() {
		return fmt.Errorf("恢复被拒绝：I/O 仍不健康")
	}
	probeIn, err := r.io.Read(r.prog.InputSize)
	if err != nil {
		return fmt.Errorf("恢复被拒绝：输入采样失败: %w", err)
	}
	safe := r.data.Clone()
	r.applySafeOutputsOn(safe)
	if err := r.io.Write(safe.Out); err != nil {
		return fmt.Errorf("恢复被拒绝：安全输出下发失败: %w", err)
	}
	if r.fault.Kind == FaultRetain && r.store != nil {
		rec, err := Snapshot(r.committed, r.prog.Hash, r.layout.Hash(), r.layout, r.scanCount)
		if err != nil {
			return fmt.Errorf("恢复被拒绝：保持快照构造失败: %w", err)
		}
		if err := r.store.Commit(rec); err != nil {
			return fmt.Errorf("恢复被拒绝：保持存储仍不可写: %w", err)
		}
	}
	// 恢复基线：最近一次完整提交；采样一次当前输入供下一周期使用。
	r.data = r.committed.Clone()
	copy(r.data.In, probeIn)
	r.fault = nil
	return nil
}

func (r *Runtime) applySafeOutputsOn(d *DataTable) {
	for i := range d.Out {
		d.Out[i] = 0
	}
	for name, val := range r.cfg.SafeOutputs {
		a, err := ParseOperand(name)
		if err != nil {
			continue
		}
		if a.Type == TypeBit {
			_ = d.writeBit(a, val != 0)
		} else {
			_ = d.writeWord(a, val)
		}
	}
}

// Run 按标称周期反复扫描，直到 Stop 或进入故障（故障后停止节拍并返回故障）。
func (r *Runtime) Run() error {
	ticker := time.NewTicker(time.Duration(r.cfg.ScanPeriodMS) * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-r.stopCh:
			return nil
		case <-ticker.C:
			if err := r.Scan(); err != nil {
				var f *Fault
				if errors.As(err, &f) {
					return f
				}
				return err
			}
		}
	}
}

func (r *Runtime) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.stopped {
		r.stopped = true
		close(r.stopCh)
	}
}
