package plc

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

const stateMagic = "SPLCSTATE1"

// TimerState / CounterState 覆盖状态元件的全部可保持量。
type TimerState struct {
	Done  bool  `json:"done"`
	Accum int64 `json:"accum"`
}

type CounterState struct {
	Done    bool  `json:"done"`
	Current int64 `json:"current"`
}

// Manifest 标识这份保持数据对应的程序身份。
type Manifest struct {
	ProgramVersion int      `json:"program_version"`
	StructureHash  string   `json:"structure_hash"`
	Retain         []string `json:"retain"` // 已排序
}

// Snapshot 是一次完整提交的保持数据：要么整份生效，要么整份不用。
type Snapshot struct {
	Manifest Manifest                `json:"manifest"`
	Bits     map[string]bool         `json:"bits"` // M（若被配置为保持目标，预留）
	Words    map[string]int64        `json:"words"`
	Timers   map[string]TimerState   `json:"timers"`
	Counters map[string]CounterState `json:"counters"`
}

// StateStore 负责保持数据的原子写入、读回与兼容性判定。
type StateStore struct {
	path string
}

func NewStateStore(path string) *StateStore { return &StateStore{path: path} }

// Save 用“临时文件 + fsync + rename”保证写到一半的内容永不生效。
func (s *StateStore) Save(snap *Snapshot) (err error) {
	payload, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(payload)
	var buf bytes.Buffer
	buf.WriteString(stateMagic)
	var l [8]byte
	binary.BigEndian.PutUint64(l[:], uint64(len(payload)))
	buf.Write(l[:])
	buf.Write(payload)
	buf.Write(sum[:])

	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	if _, err = tmp.Write(buf.Bytes()); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path)
}

// LoadResult 说明加载结论，便于日志与测试给出确定解释。
type LoadResult struct {
	State   *Snapshot
	Present bool   // 文件是否存在
	Reason  string // 兼容/冷启动/拒绝的人类可读结论
}

// Load 读入并校验保持数据。文件不存在是确定的“冷启动”；
// 文件损坏或程序身份对不上是明确的拒绝，绝不错位加载。
func (s *StateStore) Load(programVersion int, structureHash string, retain []Ref, allowDiscard bool) (*LoadResult, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		// 无文件本身是确定的冷启动；但“程序身份”由调用方在 Reload 时另行把关，
		// 避免把“旧程序有保持文件、新程序无保持点”这种配置变化误判为兼容。
		return &LoadResult{Present: false, Reason: "无保持数据文件，按冷启动处理"}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) < len(stateMagic)+8+32 || string(data[:len(stateMagic)]) != stateMagic {
		return s.reject(allowDiscard, "保持数据文件头不合法（写入中断或文件被替换）")
	}
	n := binary.BigEndian.Uint64(data[len(stateMagic) : len(stateMagic)+8])
	end := len(stateMagic) + 8 + int(n)
	if end+32 != len(data) {
		return s.reject(allowDiscard, "保持数据长度不匹配（检测到写了一半的文件）")
	}
	payload, checksum := data[len(stateMagic)+8:end], data[end:end+32]
	sum := sha256.Sum256(payload)
	if !bytes.Equal(sum[:], checksum) {
		return s.reject(allowDiscard, "保持数据校验和不通过（文件损坏）")
	}
	var snap Snapshot
	if err := json.Unmarshal(payload, &snap); err != nil {
		return s.reject(allowDiscard, "保持数据内容无法解析: "+err.Error())
	}

	// 程序身份三道兼容性检查，全部通过才允许恢复。
	if snap.Manifest.ProgramVersion != programVersion {
		return s.rejectKind(ErrVersionMismatch, allowDiscard, fmt.Sprintf(
			"程序版本不一致：保持数据来自 v%d，当前程序为 v%d",
			snap.Manifest.ProgramVersion, programVersion))
	}
	if snap.Manifest.StructureHash != structureHash {
		return s.rejectKind(ErrLayoutMismatch, allowDiscard,
			"程序结构摘要不一致：指令或元件绑定已变化")
	}
	want := refsToKeys(retain)
	if !sameSorted(want, snap.Manifest.Retain) {
		return s.rejectKind(ErrRetainMismatch, allowDiscard, fmt.Sprintf(
			"保持变量集合不一致：文件为 %v，当前配置为 %v", snap.Manifest.Retain, want))
	}
	return &LoadResult{State: &snap, Present: true, Reason: "兼容：恢复最近一次完整提交的保持数据"}, nil
}

func (s *StateStore) reject(allowDiscard bool, why string) (*LoadResult, error) {
	if allowDiscard {
		return &LoadResult{
			Present: false,
			Reason:  why + "；allow_discard_state=true，明确丢弃旧数据，按冷启动处理",
		}, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrStateCorrupt, why)
}

// rejectKind 在拒绝时保留具体的不匹配类别，便于 errors.Is 区分。
func (s *StateStore) rejectKind(kind error, allowDiscard bool, why string) (*LoadResult, error) {
	if allowDiscard {
		return &LoadResult{
			Present: false,
			Reason:  why + "；allow_discard_state=true，明确丢弃旧数据，按冷启动处理",
		}, nil
	}
	return nil, fmt.Errorf("%w (%w): %s", ErrStateCorrupt, kind, why)
}

func refsToKeys(refs []Ref) []string {
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.String()
	}
	sort.Strings(out)
	return out
}

func sameSorted(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
