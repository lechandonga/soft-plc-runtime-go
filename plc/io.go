package plc

import (
	"fmt"
	"sync"
	"time"
)

// IOModule 是输入采样/输出提交的抽象。本地用 SimIO 模拟，
// 真实硬件可实现同一接口接入；runtime 只通过它接触现场。
type IOModule interface {
	// ReadInputs 返回输入点快照。返回错误即视为本周期 I/O 失联。
	ReadInputs(timeout time.Duration) (map[Ref]bool, error)
	// WriteOutputs 整体提交一个周期的输出快照。
	WriteOutputs(outputs map[Ref]bool, timeout time.Duration) error
}

// SimIO 是纯内存的本地模拟 I/O 模块。
// 输入可由外部测试/脚本线程并发写入；ReadInputs 返回的是快照拷贝，
// runtime 在采样瞬间拿到的视图在本周期内不变。
type SimIO struct {
	mu sync.Mutex

	inputs      map[Ref]bool
	lastOutputs map[Ref]bool

	offline    bool
	readDelay  time.Duration
	writeDelay time.Duration
	failReads  int // 连续失败计数注入：>0 时下一次读失败并减一
	failWrites int
}

func NewSimIO() *SimIO {
	return &SimIO{inputs: map[Ref]bool{}, lastOutputs: map[Ref]bool{}}
}

// SetInput 由“现场线程”调用，模拟现场信号抖动，与扫描并发安全。
func (s *SimIO) SetInput(r Ref, v bool) {
	s.mu.Lock()
	s.inputs[r] = v
	s.mu.Unlock()
}

// LastOutput 返回上次成功提交的输出（供脚本/测试观察）。
func (s *SimIO) LastOutput(r Ref) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastOutputs[r]
}

func (s *SimIO) OutputsSnapshot() map[Ref]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[Ref]bool, len(s.lastOutputs))
	for k, v := range s.lastOutputs {
		out[k] = v
	}
	return out
}

// SetOffline 模拟 I/O 模块掉线/恢复（恢复后仍需 runtime 显式复位）。
func (s *SimIO) SetOffline(off bool) {
	s.mu.Lock()
	s.offline = off
	s.mu.Unlock()
}

func (s *SimIO) IsOffline() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.offline
}

// SetDelay 模拟总线延迟，配合较小 timeout 可确定性地制造 I/O 超时。
func (s *SimIO) SetDelay(d time.Duration) {
	s.mu.Lock()
	s.readDelay, s.writeDelay = d, d
	s.mu.Unlock()
}

// FailNext 安排接下来 n 次读或写返回错误。
func (s *SimIO) FailNext(reads, writes int) {
	s.mu.Lock()
	s.failReads += reads
	s.failWrites += writes
	s.mu.Unlock()
}

func (s *SimIO) ReadInputs(timeout time.Duration) (map[Ref]bool, error) {
	s.mu.Lock()
	offline := s.offline
	delay := s.readDelay
	fail := s.failReads > 0
	if fail {
		s.failReads--
	}
	s.mu.Unlock()

	if offline {
		return nil, fmt.Errorf("%w: 模拟 I/O 处于掉线状态", ErrIOLost)
	}
	if fail {
		return nil, fmt.Errorf("%w: 模拟读故障注入", ErrIOLost)
	}
	if delay > 0 {
		time.Sleep(delay)
		if delay > timeout {
			return nil, fmt.Errorf("%w: 读耗时 %s 超过超时 %s", ErrIOLost, delay, timeout)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := make(map[Ref]bool, len(s.inputs))
	for k, v := range s.inputs {
		snap[k] = v
	}
	return snap, nil
}

func (s *SimIO) WriteOutputs(outputs map[Ref]bool, timeout time.Duration) error {
	s.mu.Lock()
	offline := s.offline
	delay := s.writeDelay
	fail := s.failWrites > 0
	if fail {
		s.failWrites--
	}
	s.mu.Unlock()

	if offline {
		return fmt.Errorf("%w: 模拟 I/O 处于掉线状态", ErrIOLost)
	}
	if fail {
		return fmt.Errorf("%w: 模拟写故障注入", ErrIOLost)
	}
	if delay > 0 {
		time.Sleep(delay)
		if delay > timeout {
			return fmt.Errorf("%w: 写耗时 %s 超过超时 %s", ErrIOLost, delay, timeout)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	commit := make(map[Ref]bool, len(outputs))
	for k, v := range outputs {
		commit[k] = v
	}
	s.lastOutputs = commit
	return nil
}
