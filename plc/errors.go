package plc

import (
	"errors"
	"fmt"
)

// 按原因区分的哨兵错误，errors.Is 可精确判定拒绝原因。
var (
	ErrSyntax              = errors.New("语法错误")
	ErrUnknownInstruction  = errors.New("未知指令")
	ErrInvalidAddress      = errors.New("地址非法")
	ErrInvalidTag          = errors.New("标签非法")
	ErrDuplicateDefinition = errors.New("重复定义")
	ErrMissingReference    = errors.New("引用缺失")
	ErrInvalidConfig       = errors.New("配置非法")
	ErrStateCorrupt        = errors.New("保持数据损坏")
	ErrVersionMismatch     = errors.New("程序版本不一致")
	ErrLayoutMismatch      = errors.New("程序结构不一致")
	ErrRetainMismatch      = errors.New("保持变量集合不一致")
	ErrFaultActive         = errors.New("运行时处于故障状态")
	ErrIOLost              = errors.New("I/O 失联")
	ErrWatchdog            = errors.New("单周期看门狗超时")
	ErrExecException       = errors.New("程序执行异常")
	ErrStateFault          = errors.New("保持数据写入失败")
)

// LoadError 描述一次程序加载被拒绝的精确原因与位置。
type LoadError struct {
	Kind error  // 取上面的哨兵错误之一
	Line int    // 程序文件行号，从 1 开始
	Text string // 原始行内容
	Why  string
}

func (e *LoadError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("%s: 第 %d 行 %q: %s", e.Kind, e.Line, e.Text, e.Why)
	}
	return fmt.Sprintf("%s: %s", e.Kind, e.Why)
}

func (e *LoadError) Unwrap() error { return e.Kind }

func loadErr(kind error, line int, text, why string) *LoadError {
	return &LoadError{Kind: kind, Line: line, Text: text, Why: why}
}
