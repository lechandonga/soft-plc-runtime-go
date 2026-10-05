package plc

import (
	"bufio"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
)

// Instruction 是指令操作码。
type Instruction string

const (
	InsLD  Instruction = "LD"
	InsLDI Instruction = "LDI"
	InsAND Instruction = "AND"
	InsANI Instruction = "ANI"
	InsOR  Instruction = "OR"
	InsORI Instruction = "ORI"
	InsOUT Instruction = "OUT"
	InsRST Instruction = "RST"
	InsSET Instruction = "SET"
	InsEU  Instruction = "EU"
	InsED  Instruction = "ED"
	InsMOV Instruction = "MOV"
	InsCMP Instruction = "CMP"
	InsJMP Instruction = "JMP"
	InsTON Instruction = "TON"
	InsCTU Instruction = "CTU"
	InsEND Instruction = "END"
)

// Statement 是一条指令行（标签单独登记在 Program.Labels）。
type Statement struct {
	LineNo int
	Source string
	Op     Instruction
	Tokens []string
	Args   []Operand
	Relop  string
	JumpTo string
}

// Program 是通过全部启动前校验的控制程序，构建后不可变。
type Program struct {
	Source     string
	Statements []Statement
	Labels     map[string]int

	InputSize  int
	OutputSize int
	MemorySize int
	TimerCount int
	CounterCnt int

	Hash [32]byte
}

// LoadError 按原因分类拒绝加载。
type LoadError struct {
	Reason string
	LineNo int
	Detail string
}

func (e *LoadError) Error() string {
	if e.LineNo > 0 {
		return fmt.Sprintf("程序加载被拒绝[%s] 第%d行: %s", e.Reason, e.LineNo, e.Detail)
	}
	return fmt.Sprintf("程序加载被拒绝[%s]: %s", e.Reason, e.Detail)
}

func loadErr(reason string, line int, format string, a ...any) *LoadError {
	return &LoadError{Reason: reason, LineNo: line, Detail: fmt.Sprintf(format, a...)}
}

var instrArity = map[Instruction]int{
	InsLD: 1, InsLDI: 1, InsAND: 1, InsANI: 1, InsOR: 1, InsORI: 1,
	InsOUT: 1, InsRST: 1, InsSET: 1,
	InsEU: 0, InsED: 0, InsEND: 0,
	InsMOV: 2, InsCMP: 3, InsJMP: 1,
	InsTON: 2, InsCTU: 4,
}

// Parse 执行词法、语法、地址、引用、结构的全部启动前校验。
func Parse(source string) (*Program, error) {
	p := &Program{Source: source, Labels: map[string]int{}, Hash: sha256.Sum256([]byte(source))}

	scanner := bufio.NewScanner(strings.NewReader(source))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	lineNo := 0
	seenEnd := false
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if i := strings.Index(line, "//"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		if seenEnd {
			return nil, loadErr("StructureError", lineNo, "END 之后不允许出现语句: %q", line)
		}
		if strings.HasSuffix(line, ":") {
			label := strings.TrimSpace(strings.TrimSuffix(line, ":"))
			if !validName(label) {
				return nil, loadErr("Syntax", lineNo, "标签名 %q 非法", label)
			}
			if _, dup := p.Labels[label]; dup {
				return nil, loadErr("ReferenceError", lineNo, "标签 %q 重复定义", label)
			}
			p.Labels[label] = len(p.Statements)
			continue
		}

		fields := strings.Fields(line)
		op := Instruction(strings.ToUpper(fields[0]))
		arity, known := instrArity[op]
		if !known {
			return nil, loadErr("UnknownInstruction", lineNo, "未知指令 %q", fields[0])
		}
		if len(fields)-1 != arity {
			return nil, loadErr("Syntax", lineNo, "指令 %s 需要 %d 个操作数，实际 %d 个", op, arity, len(fields)-1)
		}
		st := Statement{LineNo: lineNo, Source: line, Op: op, Tokens: fields[1:]}
		if err := bindStatement(p, &st); err != nil {
			return nil, err
		}
		if op == InsEND {
			seenEnd = true
		}
		p.Statements = append(p.Statements, st)
	}
	if err := scanner.Err(); err != nil {
		return nil, loadErr("Syntax", lineNo, "读取程序源失败: %v", err)
	}
	if len(p.Statements) == 0 {
		return nil, loadErr("StructureError", 0, "程序为空")
	}
	last := p.Statements[len(p.Statements)-1]
	if last.Op != InsEND {
		return nil, loadErr("StructureError", last.LineNo, "程序必须以 END 结尾")
	}
	if n := countOp(p, InsEND); n != 1 {
		return nil, loadErr("StructureError", 0, "程序只允许一个 END，发现 %d 个", n)
	}
	if err := p.validateReferences(); err != nil {
		return nil, err
	}
	return p, nil
}

func validName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

func countOp(p *Program, op Instruction) int {
	n := 0
	for _, st := range p.Statements {
		if st.Op == op {
			n++
		}
	}
	return n
}

func bindStatement(p *Program, st *Statement) error {
	line := st.LineNo
	parseOps := func(indexes ...int) error {
		if len(st.Args) < len(st.Tokens) {
			st.Args = make([]Operand, len(st.Tokens))
		}
		for _, idx := range indexes {
			a, err := ParseOperand(st.Tokens[idx])
			if err != nil {
				return loadErr("IllegalAddress", line, "%v", err)
			}
			st.Args[idx] = a
		}
		return nil
	}
	bitOp := func(idx int) (Operand, error) {
		a := st.Args[idx]
		if a.Type != TypeBit {
			return a, loadErr("TypeError", line, "指令 %s 第%d个操作数必须是位地址，得到 %q", st.Op, idx+1, a.Raw)
		}
		return a, nil
	}
	wordOp := func(idx int, allowImm bool) (Operand, error) {
		a := st.Args[idx]
		if a.Kind == OpImmediate {
			if !allowImm {
				return a, loadErr("TypeError", line, "指令 %s 第%d个操作数不能是立即数", st.Op, idx+1)
			}
			return a, nil
		}
		if a.Type != TypeWord {
			return a, loadErr("TypeError", line, "指令 %s 第%d个操作数必须是字地址，得到 %q", st.Op, idx+1, a.Raw)
		}
		return a, nil
	}

	switch st.Op {
	case InsLD, InsLDI, InsAND, InsANI, InsOR, InsORI:
		if err := parseOps(0); err != nil {
			return err
		}
		if _, err := bitOp(0); err != nil {
			return err
		}
	case InsOUT, InsSET:
		if err := parseOps(0); err != nil {
			return err
		}
		a, err := bitOp(0)
		if err != nil {
			return err
		}
		if !a.Writable() {
			return loadErr("IllegalAddress", line, "指令 %s 的目标 %q 不可写", st.Op, a.Raw)
		}
	case InsRST:
		if err := parseOps(0); err != nil {
			return err
		}
		a := st.Args[0]
		if a.Kind == OpImmediate || a.Kind == OpInput {
			return loadErr("IllegalAddress", line, "RST 目标 %q 必须是可写的位/字地址", a.Raw)
		}
		if a.Type != TypeBit && a.Type != TypeWord {
			return loadErr("TypeError", line, "RST 目标 %q 类型非法", a.Raw)
		}
	case InsMOV:
		if err := parseOps(0, 1); err != nil {
			return err
		}
		if _, err := wordOp(0, true); err != nil {
			return err
		}
		dst, err := wordOp(1, false)
		if err != nil {
			return err
		}
		if !dst.Writable() {
			return loadErr("IllegalAddress", line, "MOV 目标 %q 不可写", dst.Raw)
		}
	case InsCMP:
		relop := st.Tokens[0]
		switch relop {
		case "==", "!=", ">", "<", ">=", "<=":
		default:
			return loadErr("Syntax", line, "CMP 比较符 %q 非法（== != > < >= <=）", relop)
		}
		st.Relop = relop
		if err := parseOps(1, 2); err != nil {
			return err
		}
		if _, err := wordOp(1, true); err != nil {
			return err
		}
		if _, err := wordOp(2, true); err != nil {
			return err
		}
	case InsJMP:
		label := st.Tokens[0]
		if !validName(label) {
			return loadErr("Syntax", line, "跳转标签 %q 非法", label)
		}
		st.JumpTo = label
	case InsTON:
		if err := parseOps(0, 1); err != nil {
			return err
		}
		if st.Args[0].Kind != OpTimer {
			return loadErr("TypeError", line, "TON 第1个操作数必须是 Tn，得到 %q", st.Args[0].Raw)
		}
		if st.Args[1].Kind != OpImmediate || st.Args[1].IntVal < 0 {
			return loadErr("TypeError", line, "TON 预设时间必须是非负立即数（毫秒），得到 %q", st.Tokens[1])
		}
	case InsCTU:
		if err := parseOps(0, 1, 2, 3); err != nil {
			return err
		}
		if st.Args[0].Kind != OpCounter {
			return loadErr("TypeError", line, "CTU 第1个操作数必须是 Cn，得到 %q", st.Args[0].Raw)
		}
		if st.Args[1].Kind != OpImmediate || st.Args[1].IntVal < 0 {
			return loadErr("TypeError", line, "CTU 预设值必须是非负立即数，得到 %q", st.Tokens[1])
		}
		if _, err := bitOp(2); err != nil {
			return err
		}
		if _, err := bitOp(3); err != nil {
			return err
		}
	}

	for _, a := range st.Args {
		var endBytes int
		switch a.Kind {
		case OpInput, OpOutput, OpMemory:
			if a.Type == TypeBit {
				endBytes = a.Offset/8 + 1
			} else {
				endBytes = a.Offset/8 + a.Size/8
			}
		}
		switch a.Kind {
		case OpInput:
			if endBytes > p.InputSize {
				p.InputSize = endBytes
			}
		case OpOutput:
			if endBytes > p.OutputSize {
				p.OutputSize = endBytes
			}
		case OpMemory:
			if endBytes > p.MemorySize {
				p.MemorySize = endBytes
			}
		case OpTimer:
			if a.Offset+1 > p.TimerCount {
				p.TimerCount = a.Offset + 1
			}
		case OpCounter:
			if a.Offset+1 > p.CounterCnt {
				p.CounterCnt = a.Offset + 1
			}
		}
	}
	return nil
}

func (p *Program) validateReferences() error {
	for _, st := range p.Statements {
		if st.Op == InsJMP {
			if _, ok := p.Labels[st.JumpTo]; !ok {
				return loadErr("UnknownLabel", st.LineNo, "跳转标签 %q 未定义", st.JumpTo)
			}
		}
	}
	return nil
}

// RetainEntry 是一个可保持数据项的规范描述。
type RetainEntry struct {
	Name   string
	Kind   OperandKind
	Offset int
	Size   int
}

// RetainLayout 是保持区的有序布局；名称集合与顺序完全决定二进制兼容。
type RetainLayout struct {
	Entries []RetainEntry
}

// BuildRetainLayout 依据配置的保持项名称构造布局；名称非法或重复即拒绝。
func (p *Program) BuildRetainLayout(names []string) (RetainLayout, error) {
	l := RetainLayout{}
	seen := map[string]bool{}
	for _, raw := range names {
		a, err := ParseOperand(raw)
		if err != nil {
			return l, loadErr("IllegalAddress", 0, "保持项 %q 非法: %v", raw, err)
		}
		switch a.Kind {
		case OpInput, OpImmediate, OpTimer:
			return l, loadErr("IllegalAddress", 0, "保持项 %q 不可保持（仅 M/Q 位与字、C 计数器可保持）", raw)
		}
		if a.Type != TypeBit && a.Type != TypeWord && a.Kind != OpCounter {
			return l, loadErr("IllegalAddress", 0, "保持项 %q 类型非法", raw)
		}
		key := canonicalKey(a)
		if seen[key] {
			return l, loadErr("ReferenceError", 0, "保持项 %q 重复或与其他保持项重叠", raw)
		}
		if err := p.checkAddressWithinProgram(a); err != nil {
			return l, err
		}
		seen[key] = true
		l.Entries = append(l.Entries, RetainEntry{
			Name: a.Raw, Kind: a.Kind, Offset: a.Offset, Size: a.Size,
		})
	}
	sort.SliceStable(l.Entries, func(i, j int) bool {
		if l.Entries[i].Kind != l.Entries[j].Kind {
			return l.Entries[i].Kind < l.Entries[j].Kind
		}
		return l.Entries[i].Offset < l.Entries[j].Offset
	})
	return l, nil
}

func canonicalKey(a Operand) string {
	return fmt.Sprintf("%d:%d:%d", a.Kind, a.Offset, a.Size)
}

func (p *Program) checkAddressWithinProgram(a Operand) error {
	var end int
	switch a.Kind {
	case OpMemory:
		if a.Type == TypeBit {
			end = a.Offset/8 + 1
		} else {
			end = a.Offset/8 + a.Size/8
		}
		if end > p.MemorySize {
			return loadErr("ReferenceError", 0, "保持项 %q 在程序中从未被引用", a.Raw)
		}
	case OpOutput:
		if a.Type == TypeBit {
			end = a.Offset/8 + 1
		} else {
			end = a.Offset/8 + a.Size/8
		}
		if end > p.OutputSize {
			return loadErr("ReferenceError", 0, "保持项 %q 在程序中从未被引用", a.Raw)
		}
	case OpCounter:
		if a.Offset >= p.CounterCnt {
			return loadErr("ReferenceError", 0, "保持项 %q 在程序中从未被引用", a.Raw)
		}
	}
	return nil
}

// LayoutHash 对保持布局做指纹：程序版本或保持项增删改都改变它。
func (l RetainLayout) Hash() [32]byte {
	h := sha256.New()
	for _, e := range l.Entries {
		fmt.Fprintf(h, "%s|%d|%d|%d\n", e.Name, e.Kind, e.Offset, e.Size)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}
