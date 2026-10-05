// Package simio 提供纯本地的模拟 I/O 模块，用来代替真实控制器与现场设备。
package simio

import (
	"fmt"
	"sync"
)

// Module 是运行时眼中的 I/O 模块抽象；真实硬件在生产环境可替换实现。
type Module interface {
	// Read 返回输入过程映像；实现必须返回完整快照，不允许读到半更新数据。
	Read(size int) ([]byte, error)
	// Write 原子提交整包输出。
	Write(out []byte) error
	// Healthy 报告通信是否正常。
	Healthy() bool
}

// DisconnectedError 表示 I/O 失联，运行时据此进入可区分的 IOFault。
type DisconnectedError struct{ Detail string }

func (e *DisconnectedError) Error() string { return "I/O 失联: " + e.Detail }

// SimModule 是内存模拟模块，自带断线开关与逐拍输入脚本。
type SimModule struct {
	mu sync.Mutex

	inputBytes  []byte
	outputBytes []byte
	connected   bool

	// InputScript 按扫描周期给出输入（map：位偏移 -> 值；或字节覆盖）。
	// 每拍 BeginCycle 后调用 SetInputsFromScript 也可，运行时只调 Read。
	script       [][]byte
	scriptPos    int
	holdLastWhen bool
}

func NewSimModule(inputSize, outputSize int) *SimModule {
	return &SimModule{
		inputBytes:  make([]byte, inputSize),
		outputBytes: make([]byte, outputSize),
		connected:   true,
	}
}

func (m *SimModule) Read(size int) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.connected {
		return nil, &DisconnectedError{Detail: "模拟模块处于断线状态"}
	}
	if len(m.inputBytes) < size {
		return nil, fmt.Errorf("模拟输入区不足：需要 %d 字节，实际 %d", size, len(m.inputBytes))
	}
	out := make([]byte, size)
	copy(out, m.inputBytes[:size])
	return out, nil
}

func (m *SimModule) Write(out []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.connected {
		return &DisconnectedError{Detail: "模拟模块处于断线状态"}
	}
	m.outputBytes = append([]byte(nil), out...)
	return nil
}

func (m *SimModule) Healthy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.connected
}

// SetInput 原子设置一个输入位（I byte.bit）。
func (m *SimModule) SetBit(byteIdx, bitIdx int, v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if byteIdx >= len(m.inputBytes) {
		return
	}
	mask := byte(1 << uint(bitIdx))
	if v {
		m.inputBytes[byteIdx] |= mask
	} else {
		m.inputBytes[byteIdx] &^= mask
	}
}

// SetInputByte 原子覆盖一个输入字节。
func (m *SimModule) SetInputByte(idx int, v byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if idx < len(m.inputBytes) {
		m.inputBytes[idx] = v
	}
}

// LoadInputs 原子装入整包输入（模拟现场设备批量上报）。
func (m *SimModule) LoadInputs(in []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := copy(m.inputBytes, in)
	for i := n; i < len(m.inputBytes); i++ {
		m.inputBytes[i] = 0
	}
}

func (m *SimModule) Outputs() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]byte(nil), m.outputBytes...)
}

// Inputs 返回当前输入映像副本。
func (m *SimModule) Inputs() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]byte(nil), m.inputBytes...)
}

func (m *SimModule) OutputBit(byteIdx, bitIdx int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if byteIdx >= len(m.outputBytes) {
		return false
	}
	return m.outputBytes[byteIdx]&(1<<uint(bitIdx)) != 0
}

// Disconnect / Reconnect 模拟通信通断。
func (m *SimModule) Disconnect() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.connected = false
}

func (m *SimModule) Reconnect() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.connected = true
}
