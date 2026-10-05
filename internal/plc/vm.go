package plc

import (
	"fmt"
)

// TimerState 是定时器元件状态。
type TimerState struct {
	PT      int64 // 预设（毫秒）
	Elapsed int64 // 累计计时（毫秒）
	Q       bool
}

// CounterState 是计数器元件状态。
type CounterState struct {
	PV int64
	CV int64
	Q  bool
}

// DataTable 是一个扫描周期使用的全部过程映像与状态元件。
type DataTable struct {
	In  []byte
	Out []byte
	Mem []byte

	Timers   []TimerState
	Counters []CounterState

	// EdgePrev 按“指令序号”保存上一周期 CR，边沿语义与程序结构一一绑定。
	EdgePrev []bool
	// CounterPrev 按指令序号保存上一周期 CTU 计数输入。
	CounterPrev []bool

	CR bool // 当前结果位（累加器位）
}

func newDataTable(p *Program) *DataTable {
	return &DataTable{
		In:          make([]byte, p.InputSize),
		Out:         make([]byte, p.OutputSize),
		Mem:         make([]byte, p.MemorySize),
		Timers:      make([]TimerState, p.TimerCount),
		Counters:    make([]CounterState, p.CounterCnt),
		EdgePrev:    make([]bool, len(p.Statements)),
		CounterPrev: make([]bool, len(p.Statements)),
	}
}

// Clone 生成完整深拷贝，用于故障回滚与保持快照。
func (d *DataTable) Clone() *DataTable {
	return &DataTable{
		In:          append([]byte(nil), d.In...),
		Out:         append([]byte(nil), d.Out...),
		Mem:         append([]byte(nil), d.Mem...),
		Timers:      append([]TimerState(nil), d.Timers...),
		Counters:    append([]CounterState(nil), d.Counters...),
		EdgePrev:    append([]bool(nil), d.EdgePrev...),
		CounterPrev: append([]bool(nil), d.CounterPrev...),
		CR:          d.CR,
	}
}

func (d *DataTable) readBit(a Operand) (bool, error) {
	switch a.Kind {
	case OpTimer:
		if a.Field == "Q" {
			return d.Timers[a.Offset].Q, nil
		}
	case OpCounter:
		if a.Field == "Q" {
			return d.Counters[a.Offset].Q, nil
		}
	}
	byteIdx := a.Offset / 8
	var buf []byte
	switch a.Kind {
	case OpInput:
		buf = d.In
	case OpOutput:
		buf = d.Out
	case OpMemory:
		buf = d.Mem
	default:
		return false, fmt.Errorf("内部错误：不能按位读取 %q", a.Raw)
	}
	if byteIdx >= len(buf) {
		return false, fmt.Errorf("地址 %q 越界（数据区长度 %d 字节）", a.Raw, len(buf))
	}
	return buf[byteIdx]&(1<<uint(a.Offset%8)) != 0, nil
}

func (d *DataTable) writeBit(a Operand, v bool) error {
	byteIdx := a.Offset / 8
	var buf []byte
	switch a.Kind {
	case OpOutput:
		buf = d.Out
	case OpMemory:
		buf = d.Mem
	default:
		return fmt.Errorf("内部错误：不能按位写入 %q", a.Raw)
	}
	if byteIdx >= len(buf) {
		return fmt.Errorf("地址 %q 越界（数据区长度 %d 字节）", a.Raw, len(buf))
	}
	mask := byte(1 << uint(a.Offset%8))
	if v {
		buf[byteIdx] |= mask
	} else {
		buf[byteIdx] &^= mask
	}
	return nil
}

func (d *DataTable) readWord(a Operand) (int64, error) {
	if a.Kind == OpImmediate {
		return a.IntVal, nil
	}
	switch a.Kind {
	case OpTimer:
		switch a.Field {
		case "ET":
			return d.Timers[a.Offset].Elapsed, nil
		case "PT":
			return d.Timers[a.Offset].PT, nil
		}
	case OpCounter:
		switch a.Field {
		case "CV":
			return d.Counters[a.Offset].CV, nil
		case "PV":
			return d.Counters[a.Offset].PV, nil
		}
	}
	var buf []byte
	switch a.Kind {
	case OpInput:
		buf = d.In
	case OpOutput:
		buf = d.Out
	case OpMemory:
		buf = d.Mem
	default:
		return 0, fmt.Errorf("内部错误：不能按字读取 %q", a.Raw)
	}
	start := a.Offset / 8
	n := a.Size / 8
	if start+n > len(buf) {
		return 0, fmt.Errorf("地址 %q 越界（数据区长度 %d 字节）", a.Raw, len(buf))
	}
	var u uint64
	for i := 0; i < n; i++ {
		u |= uint64(buf[start+i]) << uint(8*i)
	}
	if a.Size < 64 && u&(1<<uint(a.Size-1)) != 0 {
		u |= ^uint64(0) << uint(a.Size)
	}
	return int64(u), nil
}

func (d *DataTable) writeWord(a Operand, v int64) error {
	var buf []byte
	switch a.Kind {
	case OpOutput:
		buf = d.Out
	case OpMemory:
		buf = d.Mem
	default:
		return fmt.Errorf("内部错误：不能按字写入 %q", a.Raw)
	}
	start := a.Offset / 8
	n := a.Size / 8
	if start+n > len(buf) {
		return fmt.Errorf("地址 %q 越界（数据区长度 %d 字节）", a.Raw, len(buf))
	}
	for i := 0; i < n; i++ {
		buf[start+i] = byte(v >> uint(8*i))
	}
	return nil
}
