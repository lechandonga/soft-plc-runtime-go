package plc

import (
	"strings"
	"testing"
)

// caseLog 统一输出“业务覆盖点 / 输入 / 判定依据”，方便单测日志审计。
func caseLog(t *testing.T, business, input, expect string) {
	t.Helper()
	t.Logf("[业务] %s\n        输入: %s\n        判定: %s", business, input, expect)
}

const goodProgram = `
LD I0.0
OR Q0.0
ANI I0.1
OUT Q0.0
LD I0.2
CTU C0 3 I0.2 I0.3
LD C0.Q
OUT Q0.1
CMP != MW0 0
JMP done
MOV 42 MW0
done:
END
`

func TestParseAcceptsValidProgram(t *testing.T) {
	caseLog(t, "启动前校验-合法程序整体通过", "含位逻辑/计数器/比较/跳转/END 的完整程序",
		"Parse 无错误，区域尺寸自动推导")
	p, err := Parse(goodProgram)
	if err != nil {
		t.Fatalf("合法程序被拒绝: %v", err)
	}
	if p.InputSize != 1 || p.OutputSize != 1 || p.MemorySize != 2 {
		t.Fatalf("区域尺寸推导错误: I=%d Q=%d M=%d", p.InputSize, p.OutputSize, p.MemorySize)
	}
}

func TestLoadRejections(t *testing.T) {
	cases := []struct {
		business string
		source   string
		reason   string
	}{
		{
			"语法错误-操作数数目不符",
			"LD\nEND\n",
			"Syntax",
		},
		{
			"语法错误-标签名非法",
			"1abc:\nEND\n",
			"Syntax",
		},
		{
			"未知指令必须拒绝",
			"FOO I0.0\nEND\n",
			"UnknownInstruction",
		},
		{
			"非法地址-位编号越界",
			"LD I0.9\nEND\n",
			"IllegalAddress",
		},
		{
			"非法地址-未知区域前缀",
			"LD X0.0\nEND\n",
			"IllegalAddress",
		},
		{
			"非法地址-字地址未对齐",
			"MOV MW1 MW3\nEND\n",
			"IllegalAddress",
		},
		{
			"引用缺失-跳转标签未定义",
			"LD I0.0\nJMP nowhere\nEND\n",
			"UnknownLabel",
		},
		{
			"类型错误-字地址用于位逻辑",
			"LD MW0\nEND\n",
			"TypeError",
		},
		{
			"类型错误-位地址用于字传送",
			"MOV I0.0 MW0\nEND\n",
			"TypeError",
		},
		{
			"结构错误-缺少 END",
			"LD I0.0\nOUT Q0.0\n",
			"StructureError",
		},
		{
			"结构错误-END 之后还有语句",
			"END\nLD I0.0\n",
			"StructureError",
		},
		{
			"引用错误-向只读输入写入",
			"LD I0.0\nOUT I0.1\nEND\n",
			"IllegalAddress",
		},
		{
			"引用错误-标签重复定义",
			"a:\na:\nEND\n",
			"ReferenceError",
		},
		{
			"定时器-PT 必须是非负立即数",
			"LD I0.0\nTON T0 MW0\nEND\n",
			"TypeError",
		},
		{
			"计数器-操作数类型必须匹配",
			"LD I0.0\nCTU C0 MW0 I0.0 I0.1\nEND\n",
			"TypeError",
		},
	}
	for _, tc := range cases {
		t.Run(tc.business, func(t *testing.T) {
			caseLog(t, tc.business, strings.ReplaceAll(tc.source, "\n", " | "),
				"必须返回 LoadError 且 Reason="+tc.reason+"，不得产生部分程序")
			_, err := Parse(tc.source)
			if err == nil {
				t.Fatalf("预期拒绝(%s)，但加载成功", tc.reason)
			}
			le, ok := err.(*LoadError)
			if !ok {
				t.Fatalf("错误类型不是 *LoadError: %T", err)
			}
			if le.Reason != tc.reason {
				t.Fatalf("原因分类错误: got %s want %s (%v)", le.Reason, tc.reason, le)
			}
		})
	}
}

func TestRetainConfigValidation(t *testing.T) {
	p, err := Parse(goodProgram)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("保持项重复拒绝", func(t *testing.T) {
		caseLog(t, "保持配置-重叠保持项拒绝", "M0.0 与 M0.0",
			"BuildRetainLayout 返回 ReferenceError")
		if _, err := p.BuildRetainLayout([]string{"M0.0", "M0.0"}); err == nil {
			t.Fatal("重复保持项应被拒绝")
		}
	})
	t.Run("保持项未在程序引用拒绝", func(t *testing.T) {
		caseLog(t, "保持配置-悬空引用拒绝", "M99.9（程序从未使用）",
			"返回 ReferenceError，不静默创建默认区")
		if _, err := p.BuildRetainLayout([]string{"M99.9"}); err == nil {
			t.Fatal("未引用地址应被拒绝")
		}
	})
	t.Run("输入区不可保持", func(t *testing.T) {
		if _, err := p.BuildRetainLayout([]string{"I0.0"}); err == nil {
			t.Fatal("输入区不应可配置为保持")
		}
	})
}
