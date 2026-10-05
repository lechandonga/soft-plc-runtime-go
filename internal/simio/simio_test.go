package simio

import "testing"

func TestSimModuleSnapshotSemantics(t *testing.T) {
	m := NewSimModule(2, 2)
	m.SetBit(0, 3, true)
	in, err := m.Read(2)
	if err != nil {
		t.Fatal(err)
	}
	if in[0] != 0x08 {
		t.Fatalf("位设置错误: %02x", in[0])
	}
	// Read 必须返回独立副本：调用方修改不影响模块内部
	in[0] = 0xFF
	in2, _ := m.Read(2)
	if in2[0] != 0x08 {
		t.Fatal("Read 返回的不是独立快照，输入可能被外部半更新污染")
	}
	if err := m.Write([]byte{0xAA, 0x55}); err != nil {
		t.Fatal(err)
	}
	m.Disconnect()
	if _, err := m.Read(2); err == nil {
		t.Fatal("断线后 Read 必须报错")
	}
	if err := m.Write([]byte{0}); err == nil {
		t.Fatal("断线后 Write 必须报错")
	}
	if m.Healthy() {
		t.Fatal("Healthy 应反映断线")
	}
	m.Reconnect()
	if !m.Healthy() {
		t.Fatal("重连后应健康")
	}
}
