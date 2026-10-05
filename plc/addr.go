package plc

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// 元件类型。
const (
	KindX = 'X' // 数字量输入
	KindY = 'Y' // 数字量输出
	KindM = 'M' // 内部位
	KindT = 'T' // 定时器（完成位/当前值）
	KindC = 'C' // 计数器（完成位/当前值）
	KindD = 'D' // 数据字
)

// 各类元件的合法下标上界（含）。
var addrBounds = map[byte]int{
	KindX: 999,
	KindY: 999,
	KindM: 999,
	KindT: 255,
	KindC: 255,
	KindD: 999,
}

// Ref 是解析后的元件引用（标签最终也解析成直址）。
type Ref struct {
	Kind byte
	Idx  int
}

func (r Ref) String() string { return fmt.Sprintf("%c%d", r.Kind, r.Idx) }

// ParseAddress 解析并校验一个直址，例如 X0、T12。
func ParseAddress(s string) (Ref, error) {
	if s == "" {
		return Ref{}, fmt.Errorf("%w: 空地址", ErrInvalidAddress)
	}
	up := strings.ToUpper(s)
	kind := up[0]
	bound, ok := addrBounds[kind]
	if !ok {
		return Ref{}, fmt.Errorf("%w: %q 前缀必须是 X/Y/M/T/C/D 之一", ErrInvalidAddress, s)
	}
	num, err := strconv.Atoi(up[1:])
	if err != nil || num < 0 || num > bound {
		return Ref{}, fmt.Errorf("%w: %q 下标必须是 0..%d 的整数", ErrInvalidAddress, s, bound)
	}
	return Ref{Kind: kind, Idx: num}, nil
}

// validTagName 与 ParseTag 共同保证标签名合法且不与直址混淆。
func validTagName(name string) bool {
	if name == "" || len(name) > 32 {
		return false
	}
	for i, r := range name {
		if r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			continue
		}
		if i > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	if _, err := ParseAddress(name); err == nil {
		return false // 不允许标签名长得像直址
	}
	return true
}

// resolveOperand 把“直址或已定义标签”解析为 Ref。
func resolveOperand(token string, tags map[string]Ref) (Ref, error) {
	if r, err := ParseAddress(token); err == nil {
		return r, nil
	} else if !errors.Is(err, ErrInvalidAddress) {
		return Ref{}, err
	}
	r, ok := tags[token]
	if !ok {
		return Ref{}, fmt.Errorf("%w: 未定义标签或非法地址 %q", ErrMissingReference, token)
	}
	return r, nil
}

// parseK 解析十进制立即数 K..，允许 0..2147483647。
func parseK(token string) (int64, error) {
	if len(token) < 2 || token[0] != 'K' {
		return 0, fmt.Errorf("%w: %q 必须形如 K100", ErrSyntax, token)
	}
	v, err := strconv.ParseInt(token[1:], 10, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("%w: %q 立即数必须是非负整数", ErrSyntax, token)
	}
	return v, nil
}
