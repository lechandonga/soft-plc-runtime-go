package plc

import (
	"errors"
	"strings"
	"testing"
)

// caseLogger 输出“业务覆盖 + 输入 + 判定依据”，满足可解释性要求。
func caseLogger(t *testing.T, business, input, basis string) {
	t.Helper()
	t.Logf("[业务] %s | [输入] %s | [判定] %s", business, input, basis)
}

func parseStr(src string) (*Program, error) {
	return ParseProgram(strings.NewReader(src))
}

const goodProgram = `
VERSION 1
TAG start = X0
TAG stop  = X1
TAG run   = Y0
LD start
SET run
LD stop
RST run
LD run
OUT Y1
END
`

func TestLoadAcceptsValidProgram(t *testing.T) {
	caseLogger(t, "加载-合法程序整体接受",
		"VERSION/TAG/SET/RST/OUT/END 完整程序",
		"解析无错误，标签 start/stop/run 均绑定到合法直址，结构哈希非空")
	p, err := parseStr(goodProgram)
	if err != nil {
		t.Fatalf("合法程序不应被拒绝: %v", err)
	}
	if p.Version != 1 {
		t.Fatalf("版本应为 1，得到 %d", p.Version)
	}
	if p.StructureHash() == "" {
		t.Fatal("结构哈希不应为空")
	}
}

func TestLoadRejectsByDistinctReason(t *testing.T) {
	cases := []struct {
		name     string
		business string
		src      string
		want     error
	}{
		{
			"语法错误:VERSION 参数缺失",
			"加载拒绝-语法错误",
			"VERSION\nLD X0\nOUT Y0\nEND\n",
			ErrSyntax,
		},
		{
			"语法错误:操作数个数不对",
			"加载拒绝-语法错误",
			"LD X0 Y1\nOUT Y0\nEND\n",
			ErrSyntax,
		},
		{
			"语法错误:缺少 END",
			"加载拒绝-语法错误",
			"LD X0\nOUT Y0\n",
			ErrSyntax,
		},
		{
			"语法错误:首指令非 LD 类",
			"加载拒绝-语法错误",
			"AND X0\nOUT Y0\nEND\n",
			ErrSyntax,
		},
		{
			"语法错误:立即数非法",
			"加载拒绝-语法错误",
			"LD X0\nTON T0 KABC\nEND\n",
			ErrSyntax,
		},
		{
			"语法错误:空程序",
			"加载拒绝-语法错误",
			"# 只有注释\n\n",
			ErrSyntax,
		},
		{
			"未知指令",
			"加载拒绝-未知指令",
			"LD X0\nFOOBAR Y0\nEND\n",
			ErrUnknownInstruction,
		},
		{
			"地址非法:前缀错误",
			"加载拒绝-地址非法",
			"LD Q0\nOUT Y0\nEND\n",
			ErrInvalidAddress,
		},
		{
			"地址非法:下标越界",
			"加载拒绝-地址非法",
			"LD X100000\nOUT Y0\nEND\n",
			ErrInvalidAddress,
		},
		{
			"地址非法:位触点引用 D 字",
			"加载拒绝-地址非法",
			"LD D0\nOUT Y0\nEND\n",
			ErrInvalidAddress,
		},
		{
			"地址非法:TON 目标不是 T",
			"加载拒绝-地址非法",
			"LD X0\nTON M0 K100\nEND\n",
			ErrInvalidAddress,
		},
		{
			"地址非法:OUT 写输入点",
			"加载拒绝-地址非法",
			"LD X0\nOUT X1\nEND\n",
			ErrInvalidAddress,
		},
		{
			"标签非法:名字像直址",
			"加载拒绝-标签非法",
			"TAG X9 = X0\nLD X9\nOUT Y0\nEND\n",
			ErrInvalidTag,
		},
		{
			"标签非法:字符不允许",
			"加载拒绝-标签非法",
			"TAG 9bad = X0\nLD 9bad\nOUT Y0\nEND\n",
			ErrInvalidTag,
		},
		{
			"引用缺失:未定义标签",
			"加载拒绝-引用缺失",
			"LD ghost\nOUT Y0\nEND\n",
			ErrMissingReference,
		},
		{
			"引用缺失:跳转标号不存在",
			"加载拒绝-引用缺失",
			"LD X0\nJMP nowhere\nLBL here\nEND\n",
			ErrMissingReference,
		},
		{
			"重复定义:标签重名",
			"加载拒绝-重复定义",
			"TAG a = X0\nTAG a = X1\nLD a\nOUT Y0\nEND\n",
			ErrDuplicateDefinition,
		},
		{
			"重复定义:标号重名",
			"加载拒绝-重复定义",
			"LD X0\nJMP p\nLBL p\nLD X1\nLBL p\nEND\n",
			ErrDuplicateDefinition,
		},
		{
			"重复定义:同一输出两处线圈",
			"加载拒绝-重复定义",
			"LD X0\nOUT Y0\nLD X1\nOUT Y0\nEND\n",
			ErrDuplicateDefinition,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			caseLogger(t, tc.business, oneLine(tc.src),
				"ParseProgram 返回的错误必须 errors.Is 到 "+tc.want.Error()+"，且 LoadError 带行号")
			_, err := parseStr(tc.src)
			if err == nil {
				t.Fatalf("期望拒绝原因 %v，但程序被接受", tc.want)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("错误原因不匹配：want %v, got %v", tc.want, err)
			}
			var le *LoadError
			if !errors.As(err, &le) {
				t.Fatalf("错误应为 *LoadError: %T", err)
			}
		})
	}
}

func TestNoPartialLoadLeavesNothing(t *testing.T) {
	caseLogger(t, "加载-不得部分加载",
		"前半段合法、末段引用未定义标签的程序",
		"ParseProgram 失败即返回 nil 程序，调用方无任何 Ops 可用")
	src := "LD X0\nOUT Y0\nLD missing\nOUT Y1\nEND\n"
	p, err := parseStr(src)
	if err == nil {
		t.Fatal("应被拒绝")
	}
	if p != nil {
		t.Fatalf("拒绝时不得返回部分程序，得到 %+v", p)
	}
}

func TestStructureHashSensitivity(t *testing.T) {
	caseLogger(t, "加载-结构摘要对绑定变化敏感",
		"同文本仅标签绑定 X0->X1 的两个版本",
		"StructureHash 必须不同；仅注释差异则相同")
	p1, err := parseStr("TAG a = X0\nLD a\nOUT Y0\nEND\n")
	if err != nil {
		t.Fatal(err)
	}
	p2, err := parseStr("TAG a = X1\nLD a\nOUT Y0\nEND\n")
	if err != nil {
		t.Fatal(err)
	}
	if p1.StructureHash() == p2.StructureHash() {
		t.Fatal("标签绑定变化必须改变结构摘要")
	}
	p3, err := parseStr("# 注释不同\nLD X0\nOUT Y0\nEND\n")
	if err != nil {
		t.Fatal(err)
	}
	p4, err := parseStr("LD X0 # 行尾注释\nOUT Y0\nEND\n")
	if err != nil {
		t.Fatal(err)
	}
	if p3.StructureHash() != p4.StructureHash() {
		t.Fatal("仅注释差异不应改变结构摘要")
	}
}

func oneLine(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), "\n", "; ")
}
