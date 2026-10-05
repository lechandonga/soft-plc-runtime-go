package plc

import (
	"errors"
	"fmt"
	"time"
)

// ScanOnce 执行一个严格的“输入采样 -> 程序执行 -> 输出提交”周期。
// 处于故障态时跳过扫描，输出维持安全值，等待显式 Reset，绝不自动恢复。
func (rt *Runtime) ScanOnce() {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	if rt.fault != nil {
		return
	}
	rt.cycle++
	cycle := rt.cycle

	// 1) 输入采样：拿到一份本周期内不变的快照。
	in, err := rt.io.ReadInputs(time.Duration(rt.cfg.IOSampleTimeoutMillis) * time.Millisecond)
	if err != nil {
		rt.enterFault(FaultIO, fmt.Sprintf("输入采样失败: %v", err))
		rt.logf("周期%d 故障: %s", cycle, rt.fault.Reason)
		return
	}
	rt.inputs = in

	// 2) 程序执行：只在内存工作区计算，外部看不到中间态。
	started := time.Now()
	out, execErr := rt.execute(cycle, in)
	if execErr != nil {
		switch {
		case errors.Is(execErr, ErrWatchdog):
			rt.enterFault(FaultWatchdog, execErr.Error())
		case errors.Is(execErr, ErrStateFault):
			rt.enterFault(FaultState, execErr.Error())
		default:
			rt.enterFault(FaultExec, execErr.Error())
		}
		rt.logf("周期%d 故障: %s", cycle, rt.fault.Reason)
		return
	}
	rt.lastScanMS = time.Since(started).Milliseconds()

	// 3) 输出提交：一次性整体提交本周期结果。
	if err := rt.io.WriteOutputs(out, time.Duration(rt.cfg.IOSampleTimeoutMillis)*time.Millisecond); err != nil {
		rt.enterFault(FaultIO, fmt.Sprintf("输出提交失败: %v", err))
		rt.logf("周期%d 故障: %s", cycle, rt.fault.Reason)
		return
	}

	// 仅在 I/O 提交成功后提交内部状态（边界：提交前崩溃则本周期状态作废）。
	rt.commitWork(out)

	// 4) 保持数据：每个周期一次完整快照，原子落盘；写失败进故障态。
	if rt.cfg.PersistEveryCycle {
		if err := rt.persist(); err != nil {
			rt.enterFault(FaultState, err.Error())
			rt.logf("周期%d 故障: %s", cycle, rt.fault.Reason)
			return
		}
	}
}

// enterFault 锁定故障状态并把输出落到安全值；首个故障原因不被覆盖。
func (rt *Runtime) enterFault(code FaultCode, reason string) {
	if rt.fault == nil {
		rt.fault = &Fault{Code: code, Cycle: rt.cycle, Reason: reason}
	}
	rt.applySafeOutputs()
}

func (rt *Runtime) applySafeOutputs() {
	safe := make(map[Ref]bool)
	for r := range rt.outputs {
		safe[r] = rt.cfg.safeValue(r)
	}
	for r := range rt.prog.OutRefs {
		if r.Kind == KindY {
			safe[r] = rt.cfg.safeValue(r)
		}
	}
	if err := rt.io.WriteOutputs(safe, time.Duration(rt.cfg.IOSampleTimeoutMillis)*time.Millisecond); err != nil {
		rt.logf("警告: 故障安全输出提交失败: %v", err)
	}
	rt.outputs = safe
}

type workState struct {
	bits     map[Ref]bool
	words    map[Ref]int64
	timers   map[Ref]*timerData
	counters map[Ref]*counterData
	rstFlag  map[Ref]bool // 本周期已被 RST 复位的 T/C 元件
}

func (rt *Runtime) execute(cycle int64, in map[Ref]bool) (map[Ref]bool, error) {
	started := time.Now()
	deadline := started.Add(time.Duration(rt.cfg.WatchdogMillis) * time.Millisecond)
	if rt.hook != nil {
		if err := rt.hook(cycle); err != nil {
			return nil, fmt.Errorf("%w: 注入异常: %v", ErrExecException, err)
		}
	}
	if time.Now().After(deadline) {
		return nil, fmt.Errorf("%w: 周期执行超过看门狗 %s", ErrWatchdog,
			time.Duration(rt.cfg.WatchdogMillis)*time.Millisecond)
	}

	// 工作区从已提交状态复制，执行中的任何异常都不会污染当前状态。
	w := workState{
		bits:     cloneBits(rt.bits),
		words:    cloneWords(rt.words),
		timers:   cloneTimers(rt.timers),
		counters: cloneCounters(rt.counters),
		rstFlag:  map[Ref]bool{},
	}

	get := func(r Ref) bool {
		switch r.Kind {
		case KindX:
			return in[r]
		case KindT:
			if t := w.timers[r]; t != nil {
				return t.done
			}
			return false
		case KindC:
			if c := w.counters[r]; c != nil {
				return c.done
			}
			return false
		default:
			return w.bits[r]
		}
	}
	contact := func(op Op) bool {
		v := get(op.Ref)
		switch op.Mnemonic {
		case "LD", "AND", "OR":
			return v
		case "LDI", "ANI", "ORI":
			return !v
		}
		// P/F 边沿：当前值与“上一周期提交值”比较；
		// 同一周期内多处引用同一触点，当前值取自同一快照，结果确定一致。
		prev := rt.prevEdge[op.Ref]
		var hit bool
		switch op.Mnemonic {
		case "LDP", "ANDP", "ORP":
			hit = v && !prev
		case "LDF", "ANDF", "ORF":
			hit = !v && prev
		}
		return hit
	}

	var stack []bool
	var budget int64

	// 扫描前预处理：TON/CTU 的累加需要用到本周期的能流，
	// 因此在解释执行时就地更新（见下），保持“逐行、顺序扫描”的确定语义。
	newEdge := map[Ref]bool{}
	edgeRefs := map[Ref]bool{}
	allOps := rt.prog.Ops
	for _, o := range allOps {
		switch o.Mnemonic {
		case "LDP", "LDF", "ANDP", "ANDF", "ORP", "ORF":
			edgeRefs[o.Ref] = true
		}
	}

	ops := allOps
	i := 0
	for i < len(ops) {
		budget++
		if budget > rt.cfg.InstructionBudget {
			return nil, fmt.Errorf("%w: 单周期指令执行数超过预算 %d", ErrWatchdog, rt.cfg.InstructionBudget)
		}
		if budget&0x7 == 0 && time.Now().After(deadline) {
			return nil, fmt.Errorf("%w: 周期执行超过看门狗 %s", ErrWatchdog,
				time.Duration(rt.cfg.WatchdogMillis)*time.Millisecond)
		}

		op := ops[i]
		switch op.Mnemonic {
		case "LD", "LDI", "LDP", "LDF":
			stack = append(stack, contact(op))
		case "AND", "ANI", "ANDP", "ANDF":
			if len(stack) == 0 {
				return nil, fmt.Errorf("%w: 第 %d 行 AND 类指令前缺少 LD", ErrExecException, op.Line)
			}
			stack[len(stack)-1] = stack[len(stack)-1] && contact(op)
		case "OR", "ORI", "ORP", "ORF":
			if len(stack) == 0 {
				return nil, fmt.Errorf("%w: 第 %d 行 OR 类指令前缺少 LD", ErrExecException, op.Line)
			}
			stack[len(stack)-1] = stack[len(stack)-1] || contact(op)
		case "OUT":
			if len(stack) == 0 {
				return nil, fmt.Errorf("%w: 第 %d 行 OUT 前缺少 LD", ErrExecException, op.Line)
			}
			w.bits[op.Ref] = top(stack)
		case "SET":
			if len(stack) == 0 {
				return nil, fmt.Errorf("%w: 第 %d 行 SET 前缺少 LD", ErrExecException, op.Line)
			}
			if top(stack) {
				w.bits[op.Ref] = true
			}
		case "RST":
			if len(stack) == 0 {
				return nil, fmt.Errorf("%w: 第 %d 行 RST 前缺少 LD", ErrExecException, op.Line)
			}
			if top(stack) {
				w.bits[op.Ref] = false
				switch op.Ref.Kind {
				case KindT:
					if t := w.timers[op.Ref]; t != nil {
						t.accum, t.done = 0, false
					}
					w.rstFlag[op.Ref] = true
				case KindC:
					if c := w.counters[op.Ref]; c != nil {
						// 只清计数与完成位；prev 保留本周期能流，
						// 避免“复位当拍 X0=1、下一拍 X0 仍为 1”被误算成上升沿。
						c.current, c.done = 0, false
					}
					w.rstFlag[op.Ref] = true
				}
			}
		case "TON":
			// TON Tn K毫秒：能流累计时间，到点完成位置位；失能清零。
			en := len(stack) > 0 && top(stack)
			if w.rstFlag[op.Ref] {
				break
			}
			t := w.timers[op.Ref]
			if t == nil {
				t = &timerData{}
				w.timers[op.Ref] = t
			}
			if en {
				t.accum += int64(rt.cfg.CycleIntervalMillis)
				if t.accum >= op.Imm {
					t.done = true
				}
			} else {
				t.accum, t.done = 0, false
			}
		case "CTU":
			// CTU Cn K预设：能流上升沿计 1，到达预设完成位置位；
			// 对同一 C 元件执行 RST Cn 清零。
			en := len(stack) > 0 && top(stack)
			if w.rstFlag[op.Ref] {
				break
			}
			c := w.counters[op.Ref]
			if c == nil {
				c = &counterData{}
				w.counters[op.Ref] = c
			}
			if en && !c.prev {
				c.current++
				if c.current >= op.Imm {
					c.done = true
				}
			}
			c.prev = en
		case "MOV":
			// MOV K值 Dn：整字写入（当前仅支持立即数，保持确定）。
			w.words[op.Ref] = op.Imm
		case "JMP":
			if len(stack) == 0 {
				return nil, fmt.Errorf("%w: 第 %d 行 JMP 前缺少 LD 能流", ErrExecException, op.Line)
			}
			if top(stack) {
				i = op.JmpIdx
				continue
			}
		case "LBL":
			// 标号占位，顺序落入下一条。
		case "END":
			i = len(ops)
			continue
		}
		i++
	}

	// 计算待提交输出：只提交程序涉及的 Y 点；未驱动 Y 沿用上次值。
	out := cloneBits(rt.outputs)
	for r := range rt.prog.OutRefs {
		if r.Kind == KindY {
			out[r] = w.bits[r]
		}
	}
	// 本周期边沿元件的当前值，在提交后成为“上一周期值”。
	for r := range edgeRefs {
		newEdge[r] = get(r)
	}

	// 暂存本周期工作副本，在 I/O 提交成功后由 commitWork 正式生效。
	rt.pending = &pendingWork{w: w, out: out, newEdge: newEdge}
	return out, nil
}

type pendingWork struct {
	w       workState
	out     map[Ref]bool
	newEdge map[Ref]bool
}

func (rt *Runtime) commitWork(out map[Ref]bool) {
	p := rt.pending
	rt.bits = p.w.bits
	rt.words = p.w.words
	rt.timers = p.w.timers
	rt.counters = p.w.counters
	rt.prevEdge = p.newEdge
	rt.outputs = out
	rt.pending = nil
}

func top(stack []bool) bool { return stack[len(stack)-1] }

func cloneBits(m map[Ref]bool) map[Ref]bool {
	c := make(map[Ref]bool, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

func cloneWords(m map[Ref]int64) map[Ref]int64 {
	c := make(map[Ref]int64, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

func cloneTimers(m map[Ref]*timerData) map[Ref]*timerData {
	c := make(map[Ref]*timerData, len(m))
	for k, v := range m {
		t := *v
		c[k] = &t
	}
	return c
}

func cloneCounters(m map[Ref]*counterData) map[Ref]*counterData {
	c := make(map[Ref]*counterData, len(m))
	for k, v := range m {
		t := *v
		c[k] = &t
	}
	return c
}
