package plc

import "fmt"

// ExecError 是程序执行阶段的确定性异常（如除零、地址越界）。
type ExecError struct {
	LineNo int
	Detail string
}

func (e *ExecError) Error() string {
	return fmt.Sprintf("执行异常 第%d行: %s", e.LineNo, e.Detail)
}

// ExecConfig 控制执行器的看门狗预算（与墙钟解耦，保证可复现）。
type ExecConfig struct {
	// MaxInstructions 为单个扫描周期允许执行的指令条数（含 JMP 后重复执行）。
	// 0 表示使用默认值。
	MaxInstructions int
}

// Execute 在给定数据表上完整执行一次程序。
// tickMS 是本周期的确定性时间增量（毫秒），供定时器累计。
// 执行过程中任何异常都会返回错误；调用方负责用周期前快照回滚。
func Execute(p *Program, d *DataTable, tickMS int64, cfg ExecConfig) (err error) {
	budget := cfg.MaxInstructions
	if budget <= 0 {
		budget = 1_000_000
	}
	used := 0
	checkBudget := func(st *Statement) error {
		used++
		if used > budget {
			return &WatchdogError{LineNo: st.LineNo, Limit: budget}
		}
		return nil
	}

	defer func() {
		if r := recover(); r != nil {
			err = &ExecError{Detail: fmt.Sprintf("内部 panic: %v", r)}
		}
	}()

	pc := 0
	d.CR = false
	for pc < len(p.Statements) {
		st := &p.Statements[pc]
		if e := checkBudget(st); e != nil {
			return e
		}
		next := pc + 1
		switch st.Op {
		case InsLD, InsLDI:
			v, e := d.readBit(st.Args[0])
			if e != nil {
				return &ExecError{LineNo: st.LineNo, Detail: e.Error()}
			}
			if st.Op == InsLDI {
				v = !v
			}
			d.CR = v
		case InsAND, InsANI, InsOR, InsORI:
			v, e := d.readBit(st.Args[0])
			if e != nil {
				return &ExecError{LineNo: st.LineNo, Detail: e.Error()}
			}
			if st.Op == InsANI || st.Op == InsORI {
				v = !v
			}
			if st.Op == InsAND || st.Op == InsANI {
				d.CR = d.CR && v
			} else {
				d.CR = d.CR || v
			}
		case InsOUT:
			if e := d.writeBit(st.Args[0], d.CR); e != nil {
				return &ExecError{LineNo: st.LineNo, Detail: e.Error()}
			}
		case InsSET:
			if d.CR {
				if e := d.writeBit(st.Args[0], true); e != nil {
					return &ExecError{LineNo: st.LineNo, Detail: e.Error()}
				}
			}
		case InsRST:
			if d.CR {
				a := st.Args[0]
				if a.Kind == OpCounter {
					d.Counters[a.Offset].CV = 0
					d.Counters[a.Offset].Q = false
				} else if a.Type == TypeBit {
					if e := d.writeBit(a, false); e != nil {
						return &ExecError{LineNo: st.LineNo, Detail: e.Error()}
					}
				} else {
					if e := d.writeWord(a, 0); e != nil {
						return &ExecError{LineNo: st.LineNo, Detail: e.Error()}
					}
				}
			}
		case InsEU:
			prev := d.EdgePrev[pc]
			d.EdgePrev[pc] = d.CR
			d.CR = d.CR && !prev
		case InsED:
			prev := d.EdgePrev[pc]
			d.EdgePrev[pc] = d.CR
			d.CR = !d.CR && prev
		case InsMOV:
			v, e := d.readWord(st.Args[0])
			if e != nil {
				return &ExecError{LineNo: st.LineNo, Detail: e.Error()}
			}
			if e := d.writeWord(st.Args[1], v); e != nil {
				return &ExecError{LineNo: st.LineNo, Detail: e.Error()}
			}
		case InsCMP:
			l, e1 := d.readWord(st.Args[1])
			r, e2 := d.readWord(st.Args[2])
			if e1 != nil || e2 != nil {
				if e1 != nil {
					return &ExecError{LineNo: st.LineNo, Detail: e1.Error()}
				}
				return &ExecError{LineNo: st.LineNo, Detail: e2.Error()}
			}
			d.CR = compare(st.Relop, l, r)
		case InsJMP:
			// 跳转指令本身不更新边沿历史。
			if d.CR {
				next = p.Labels[st.JumpTo]
			}
		case InsTON:
			a := st.Args[0]
			t := &d.Timers[a.Offset]
			t.PT = st.Args[1].IntVal
			if d.CR {
				t.Elapsed += tickMS
				if t.Elapsed >= t.PT {
					t.Q = true
				}
			} else {
				t.Elapsed = 0
				t.Q = false
			}
		case InsCTU:
			cu, e := d.readBit(st.Args[2])
			if e != nil {
				return &ExecError{LineNo: st.LineNo, Detail: e.Error()}
			}
			reset, e := d.readBit(st.Args[3])
			if e != nil {
				return &ExecError{LineNo: st.LineNo, Detail: e.Error()}
			}
			a := st.Args[0]
			c := &d.Counters[a.Offset]
			c.PV = st.Args[1].IntVal
			if reset {
				c.CV = 0
				c.Q = false
				d.CounterPrev[pc] = cu
			} else {
				prev := d.CounterPrev[pc]
				d.CounterPrev[pc] = cu
				if cu && !prev && !c.Q {
					c.CV++
				}
				if c.CV >= c.PV {
					c.Q = true
				}
			}
		case InsEND:
			return nil
		}
		pc = next
	}
	return &ExecError{Detail: "程序缺少 END（不应发生，解析期已拦截）"}
}

func compare(op string, l, r int64) bool {
	switch op {
	case "==":
		return l == r
	case "!=":
		return l != r
	case ">":
		return l > r
	case "<":
		return l < r
	case ">=":
		return l >= r
	case "<=":
		return l <= r
	}
	return false
}

// WatchdogError 表示单周期指令预算耗尽（单周期超时）。
type WatchdogError struct {
	LineNo int
	Limit  int
}

func (e *WatchdogError) Error() string {
	return fmt.Sprintf("看门狗超时：单周期指令数超过预算 %d（停在第%d行）", e.Limit, e.LineNo)
}
