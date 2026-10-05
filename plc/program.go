package plc

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Op 是一条已校验、已解析的指令。
type Op struct {
	Mnemonic string
	Line     int
	Ref      Ref
	Imm      int64
	Label    string
	JmpIdx   int // JMP 目标指令下标，未用为 -1
}

// Program 是整体加载、整体生效的控制程序。
type Program struct {
	Version   int
	Ops       []Op
	Tags      map[string]Ref
	Labels    map[string]int
	Coils     map[Ref]int  // 每个被写位元件只允许出现一次线圈
	OutRefs   map[Ref]bool // 所有可写 Y 点（OUT/SET/RST），故障时落安全值
	structure string
}

// StructureHash 对“指令结构 + 标签绑定”做归一化摘要，
// 仅指令注释/空白变化不影响，任何元件绑定变化都会改变。
func (p *Program) StructureHash() string {
	sum := sha256.Sum256([]byte(p.structure))
	return hex.EncodeToString(sum[:])
}

var knownOps = map[string]int{
	"LD": 1, "LDI": 1, "LDP": 1, "LDF": 1,
	"AND": 1, "ANI": 1, "ANDP": 1, "ANDF": 1,
	"OR": 1, "ORI": 1, "ORP": 1, "ORF": 1,
	"OUT": 1, "SET": 1, "RST": 1,
	"MOV": 2, "TON": 2, "CTU": 2,
	"JMP": 1, "LBL": 1, "END": 0,
}

var bitReadOps = map[string]bool{
	"LD": true, "LDI": true, "LDP": true, "LDF": true,
	"AND": true, "ANI": true, "ANDP": true, "ANDF": true,
	"OR": true, "ORI": true, "ORP": true, "ORF": true,
}

// ParseProgram 做完整的启动前校验：任何一条不合法都整体拒绝。
func ParseProgram(r io.Reader) (*Program, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")

	prog := &Program{
		Version: 1,
		Tags:    map[string]Ref{},
		Labels:  map[string]int{},
		Coils:   map[Ref]int{},
		OutRefs: map[Ref]bool{},
	}
	type rawOp struct {
		mnemonic string
		args     []string
		line     int
		text     string
	}
	var raws []rawOp
	labelLine := map[string]int{}

	for i, content := range lines {
		lineNo := i + 1
		text := strings.TrimSpace(content)
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		if i := indexComment(text); i >= 0 {
			text = strings.TrimSpace(text[:i])
		}
		if text == "" {
			continue
		}
		fields := strings.Fields(text)
		head := strings.ToUpper(fields[0])

		switch head {
		case "VERSION":
			if len(fields) != 2 {
				return nil, loadErr(ErrSyntax, lineNo, text, "VERSION 指令形如 VERSION 1")
			}
			v, err := strconv.Atoi(fields[1])
			if err != nil || v < 1 {
				return nil, loadErr(ErrSyntax, lineNo, text, "版本号必须是正整数")
			}
			prog.Version = v
			continue
		case "TAG":
			if len(fields) != 4 || fields[2] != "=" {
				return nil, loadErr(ErrSyntax, lineNo, text, "标签定义形如 TAG name = X0")
			}
			name := fields[1]
			if !validTagName(name) {
				return nil, loadErr(ErrInvalidTag, lineNo, text, "标签名需为字母/下划线开头的字母数字下划线，且不得与直址重名")
			}
			if _, dup := prog.Tags[name]; dup {
				return nil, loadErr(ErrDuplicateDefinition, lineNo, text, fmt.Sprintf("标签 %q 重复定义", name))
			}
			ref, perr := ParseAddress(fields[3])
			if perr != nil {
				return nil, loadErr(ErrInvalidAddress, lineNo, text, perr.Error())
			}
			prog.Tags[name] = ref
			continue
		}

		argc, known := knownOps[head]
		if !known {
			return nil, loadErr(ErrUnknownInstruction, lineNo, text, fmt.Sprintf("未知指令 %q", fields[0]))
		}
		if len(fields)-1 != argc {
			return nil, loadErr(ErrSyntax, lineNo, text, fmt.Sprintf("指令 %s 需要 %d 个操作数，实际 %d 个", head, argc, len(fields)-1))
		}
		if head == "LBL" {
			name := fields[1]
			if !validTagName(name) {
				return nil, loadErr(ErrInvalidTag, lineNo, text, "标号名需为合法标签名")
			}
			if ln, dup := labelLine[name]; dup {
				return nil, loadErr(ErrDuplicateDefinition, lineNo, text, fmt.Sprintf("标号 %q 已在第 %d 行定义", name, ln))
			}
			labelLine[name] = lineNo
		}
		var args []string
		if argc > 0 {
			args = fields[1:]
		}
		raws = append(raws, rawOp{mnemonic: head, args: args, line: lineNo, text: text})
	}

	if len(raws) == 0 {
		return nil, loadErr(ErrSyntax, 0, "", "程序为空：至少需要一条指令")
	}

	// 第二遍：操作数解析与引用完整性校验。
	var norm strings.Builder
	fmt.Fprintf(&norm, "V%d\n", prog.Version)
	tagNames := make([]string, 0, len(prog.Tags))
	for n := range prog.Tags {
		tagNames = append(tagNames, n)
	}
	sort.Strings(tagNames)
	for _, n := range tagNames {
		fmt.Fprintf(&norm, "TAG %s=%s\n", n, prog.Tags[n])
	}

	for idx, ro := range raws {
		op := Op{Mnemonic: ro.mnemonic, Line: ro.line, JmpIdx: -1}
		switch ro.mnemonic {
		case "END":
		case "LBL":
			op.Label = ro.args[0]
			prog.Labels[op.Label] = idx
		case "JMP":
			name := ro.args[0]
			if !validTagName(name) {
				return nil, loadErr(ErrInvalidTag, ro.line, ro.text, "跳转目标需为合法标号名")
			}
			if _, ok := labelLine[name]; !ok {
				return nil, loadErr(ErrMissingReference, ro.line, ro.text, fmt.Sprintf("跳转到未定义标号 %q", name))
			}
			op.Label = name
		case "TON":
			var ref Ref
			if looksLikeAddress(ro.args[0]) {
				r, aerr := ParseAddress(ro.args[0])
				if aerr != nil {
					return nil, loadErr(ErrInvalidAddress, ro.line, ro.text, aerr.Error())
				}
				ref = r
			} else {
				r, e := resolveOperand(ro.args[0], prog.Tags)
				if e != nil {
					return nil, loadErr(kindOf(e), ro.line, ro.text, e.Error())
				}
				ref = r
			}
			if ref.Kind != KindT {
				return nil, loadErr(ErrInvalidAddress, ro.line, ro.text, "TON 的目标必须是 T 元件")
			}
			imm, e := parseK(ro.args[1])
			if e != nil {
				return nil, loadErr(ErrSyntax, ro.line, ro.text, e.Error())
			}
			op.Ref, op.Imm = ref, imm
		case "CTU":
			var ref Ref
			if looksLikeAddress(ro.args[0]) {
				r, aerr := ParseAddress(ro.args[0])
				if aerr != nil {
					return nil, loadErr(ErrInvalidAddress, ro.line, ro.text, aerr.Error())
				}
				ref = r
			} else {
				r, e := resolveOperand(ro.args[0], prog.Tags)
				if e != nil {
					return nil, loadErr(kindOf(e), ro.line, ro.text, e.Error())
				}
				ref = r
			}
			if ref.Kind != KindC {
				return nil, loadErr(ErrInvalidAddress, ro.line, ro.text, "CTU 的目标必须是 C 元件")
			}
			imm, e := parseK(ro.args[1])
			if e != nil {
				return nil, loadErr(ErrSyntax, ro.line, ro.text, e.Error())
			}
			op.Ref, op.Imm = ref, imm
		case "MOV":
			imm, e := parseK(ro.args[0])
			if e != nil {
				return nil, loadErr(ErrSyntax, ro.line, ro.text, e.Error())
			}
			var ref Ref
			if looksLikeAddress(ro.args[1]) {
				r, aerr := ParseAddress(ro.args[1])
				if aerr != nil {
					return nil, loadErr(ErrInvalidAddress, ro.line, ro.text, aerr.Error())
				}
				ref = r
			} else {
				r, e := resolveOperand(ro.args[1], prog.Tags)
				if e != nil {
					return nil, loadErr(kindOf(e), ro.line, ro.text, e.Error())
				}
				ref = r
			}
			if ref.Kind != KindD {
				return nil, loadErr(ErrInvalidAddress, ro.line, ro.text, "MOV 目标必须是 D 元件")
			}
			op.Ref, op.Imm = ref, imm
		default:
			token := ro.args[0]
			var ref Ref
			var e error
			// 形态像直址（单字母前缀 + 数字）时必须按直址校验，
			// 非法下标/前缀报“地址非法”，而不是被当成未定义标签。
			if looksLikeAddress(token) {
				r, aerr := ParseAddress(token)
				if aerr != nil {
					return nil, loadErr(ErrInvalidAddress, ro.line, ro.text, aerr.Error())
				}
				ref, e = r, nil
			} else {
				ref, e = resolveOperand(token, prog.Tags)
			}
			if e != nil {
				return nil, loadErr(kindOf(e), ro.line, ro.text, e.Error())
			}
			if bitReadOps[ro.mnemonic] {
				if ref.Kind == KindD {
					return nil, loadErr(ErrInvalidAddress, ro.line, ro.text, "位触点不能引用 D 字元件")
				}
			} else { // OUT / SET / RST
				// RST Tn/Cn 是对状态元件的复位，允许与 TON/CTU 共用同一元件，
				// 不纳入“一位一线圈”去重。
				if op.Mnemonic == "RST" && (ref.Kind == KindT || ref.Kind == KindC) {
					op.Ref = ref
					break
				}
				if ref.Kind != KindY && ref.Kind != KindM {
					return nil, loadErr(ErrInvalidAddress, ro.line, ro.text,
						"OUT/SET 只能写 Y 或 M 元件；RST 还可复位 T/C 元件")
				}
				// OUT 每周期无条件覆盖结果，同一元件只允许一处（保证结果确定）；
				// SET/RST 是标准启保停组合，允许成对作用于同一线圈。
				if ro.mnemonic == "OUT" {
					if first, dup := prog.Coils[ref]; dup {
						return nil, loadErr(ErrDuplicateDefinition, ro.line, ro.text,
							fmt.Sprintf("%s 在第 %d 行已有 OUT 线圈，同一元件禁止两处 OUT", ref, first))
					}
					prog.Coils[ref] = ro.line
				}
				if ref.Kind == KindY {
					prog.OutRefs[ref] = true
				}
			}
			op.Ref = ref
		}

		switch {
		case op.Mnemonic == "LBL":
			fmt.Fprintf(&norm, "%d:LBL %s\n", idx, op.Label)
		case op.Mnemonic == "JMP":
			fmt.Fprintf(&norm, "%d:JMP %s\n", idx, op.Label)
		case len(ro.args) == 2:
			fmt.Fprintf(&norm, "%d:%s %s K%d\n", idx, op.Mnemonic, op.Ref, op.Imm)
		default:
			fmt.Fprintf(&norm, "%d:%s %s\n", idx, op.Mnemonic, op.Ref)
		}
		prog.Ops = append(prog.Ops, op)
	}

	if prog.Ops[len(prog.Ops)-1].Mnemonic != "END" {
		return nil, loadErr(ErrSyntax, 0, "", "程序必须以 END 结束")
	}
	if prog.Ops[0].Mnemonic != "LD" && prog.Ops[0].Mnemonic != "LDI" &&
		prog.Ops[0].Mnemonic != "LDP" && prog.Ops[0].Mnemonic != "LDF" &&
		prog.Ops[0].Mnemonic != "LBL" {
		return nil, loadErr(ErrSyntax, prog.Ops[0].Line, "", "程序必须以 LD/LDI/LDP/LDF 或 LBL 起始")
	}

	// JMP 目标下标回填。
	for i := range prog.Ops {
		if prog.Ops[i].Mnemonic == "JMP" {
			prog.Ops[i].JmpIdx = prog.Labels[prog.Ops[i].Label]
		}
	}
	prog.structure = norm.String()
	return prog, nil
}

func indexComment(s string) int {
	in := false
	for i, r := range s {
		switch r {
		case '"':
			in = !in
		case '#':
			if !in {
				return i
			}
		}
	}
	return -1
}

// looksLikeAddress 判断 token 是否呈“单字母前缀 + 数字尾”的直址形态，
// 这类 token 即使非法也必须按地址错误拒绝，而非当作未定义标签。
func looksLikeAddress(s string) bool {
	if len(s) < 2 {
		return false
	}
	c := s[0]
	isLetter := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
	lastDigit := s[len(s)-1] >= '0' && s[len(s)-1] <= '9'
	return isLetter && lastDigit
}

func kindOf(e error) error {
	for _, k := range []error{ErrMissingReference, ErrInvalidAddress, ErrInvalidTag} {
		if errorIs(e, k) {
			return k
		}
	}
	return ErrSyntax
}

func errorIs(err, target error) bool {
	type unwrapper interface{ Unwrap() error }
	for err != nil {
		if err == target {
			return true
		}
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
