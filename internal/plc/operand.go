package plc

import (
	"fmt"
	"strconv"
	"strings"
)

// OperandKind 标识操作数的种类。
type OperandKind int

const (
	OpInvalid   OperandKind = iota
	OpInput                 // 输入区 I
	OpOutput                // 输出区 Q
	OpMemory                // 位存储器 M
	OpTimer                 // T
	OpCounter               // C
	OpImmediate             // 立即数
)

// ValueType 是操作数的数值类型：位或 64 位有符号字（字节/字/双字统一用 int64 承载）。
type ValueType int

const (
	TypeNone ValueType = iota
	TypeBit
	TypeWord
)

// Operand 是经过词法解析但尚未绑定地址的操作数。
type Operand struct {
	Raw    string
	Kind   OperandKind
	Type   ValueType
	Area   byte // 'I' 'Q' 'M' 'T' 'C'
	Offset int  // 位偏移或字节偏移
	Size   int  // 1/8/16/32/64 位
	IntVal int64
	Field  string // Tn/Cn 的字段：Q ET PT / Q CV PV
}

// Name 返回规范名称，用于保持区布局与 I/O 映射。
func (o Operand) Name() string {
	return o.Raw
}

// Writable 报告该操作数能否作为赋值目标。
func (o Operand) Writable() bool {
	switch o.Kind {
	case OpOutput, OpMemory, OpTimer, OpCounter:
		return true
	default:
		return false
	}
}

// IsRuntimeMaintained 报告该操作数是否落在“由运行时自己维护的状态量”上
// （定时器/计数器元件及其字段）。这类对象只能由 TON/CTU/RST(整计数器) 等
// 指令内部更新，不允许作为普通写入指令的目标。
func (o Operand) IsRuntimeMaintained() bool {
	return o.Kind == OpTimer || o.Kind == OpCounter
}

// ParseOperand 解析单个操作数词法单元，只做词法与范围校验。
func ParseOperand(s string) (Operand, error) {
	if s == "" {
		return Operand{}, fmt.Errorf("空操作数")
	}
	if v, err := parseImmediate(s); err == nil {
		return Operand{Raw: s, Kind: OpImmediate, Type: TypeWord, IntVal: v, Size: 64}, nil
	}

	area := s[0]
	body := s[1:]
	switch area {
	case 'I', 'Q', 'M':
		return parseAreaOperand(s, area, body)
	case 'T', 'C':
		num := body
		field := ""
		if i := strings.Index(body, "."); i >= 0 {
			num, field = body[:i], body[i+1:]
		}
		idx, err := strconv.Atoi(num)
		if err != nil || idx < 0 || num == "" {
			return Operand{}, fmt.Errorf("操作数 %q 非法：%c 后必须是非负编号", s, area)
		}
		kind := OpTimer
		if area == 'C' {
			kind = OpCounter
		}
		if field != "" {
			var validFields map[string]bool
			if area == 'T' {
				validFields = map[string]bool{"Q": true, "ET": true, "PT": true}
			} else {
				validFields = map[string]bool{"Q": true, "CV": true, "PV": true}
			}
			if !validFields[field] {
				return Operand{}, fmt.Errorf("操作数 %q 非法：未知字段 %q", s, field)
			}
			t := TypeWord
			if field == "Q" {
				t = TypeBit
			}
			return Operand{Raw: s, Kind: kind, Type: t, Area: area, Offset: idx, Field: field, Size: 64}, nil
		}
		return Operand{Raw: s, Kind: kind, Type: TypeNone, Area: area, Offset: idx}, nil
	}
	return Operand{}, fmt.Errorf("操作数 %q 非法：未知区域前缀 %q", s, string(area))
}

func parseAreaOperand(raw string, area byte, body string) (Operand, error) {
	kind := map[byte]OperandKind{'I': OpInput, 'Q': OpOutput, 'M': OpMemory}[area]
	if strings.Contains(body, ".") {
		parts := strings.Split(body, ".")
		if len(parts) != 2 {
			return Operand{}, fmt.Errorf("位地址 %q 格式非法，应为 区域+字节.位", raw)
		}
		byteIdx, err := strconv.Atoi(parts[0])
		if err != nil || byteIdx < 0 {
			return Operand{}, fmt.Errorf("位地址 %q 字节编号非法", raw)
		}
		bitIdx, err := strconv.Atoi(parts[1])
		if err != nil || bitIdx < 0 || bitIdx > 7 {
			return Operand{}, fmt.Errorf("位地址 %q 位编号必须在 0..7", raw)
		}
		return Operand{Raw: raw, Kind: kind, Type: TypeBit, Area: area,
			Offset: byteIdx*8 + bitIdx, Size: 1}, nil
	}
	if len(body) < 2 {
		return Operand{}, fmt.Errorf("地址 %q 非法，缺少宽度前缀（B/W/D/L）", raw)
	}
	widthCh := body[0]
	width, ok := map[byte]int{'B': 8, 'W': 16, 'D': 32, 'L': 64}[widthCh]
	if !ok {
		return Operand{}, fmt.Errorf("地址 %q 宽度前缀 %q 非法（仅支持 B/W/D/L）", raw, string(widthCh))
	}
	idx, err := strconv.Atoi(body[1:])
	if err != nil || idx < 0 {
		return Operand{}, fmt.Errorf("地址 %q 字节编号非法", raw)
	}
	// 宽度必须按字节对齐（与区域布局一一对应，避免重叠歧义）。
	if width > 8 && idx%(width/8) != 0 {
		return Operand{}, fmt.Errorf("地址 %q 未按 %d 位边界对齐", raw, width)
	}
	return Operand{Raw: raw, Kind: kind, Type: TypeWord, Area: area,
		Offset: idx * 8, Size: width}, nil
}

func parseImmediate(s string) (int64, error) {
	switch {
	case strings.HasPrefix(s, "16#"):
		return strconv.ParseInt(s[3:], 16, 64)
	case strings.HasPrefix(s, "2#"):
		return strconv.ParseInt(s[2:], 2, 64)
	case strings.HasPrefix(s, "-"):
		if _, err := strconv.ParseInt(s, 10, 64); err != nil {
			return 0, err
		}
		return strconv.ParseInt(s, 10, 64)
	default:
		if v, err := strconv.ParseInt(s, 10, 64); err == nil {
			return v, nil
		}
		return 0, fmt.Errorf("不是立即数 %q", s)
	}
}
