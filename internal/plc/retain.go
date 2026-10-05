package plc

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// retainMagic 是保持数据文件的固定魔数。
const retainMagic = "SPLCRET1"

// RetainMismatchError 表示程序版本或保持布局与持久化数据对不上。
type RetainMismatchError struct {
	Kind   string // ProgramChanged | LayoutChanged | Corrupt
	Detail string
}

func (e *RetainMismatchError) Error() string {
	return fmt.Sprintf("保持数据不兼容[%s]: %s", e.Kind, e.Detail)
}

// RetainStore 以“临时文件 + 原子改名 + 校验和”方式持久化保持数据。
// 写入一半时崩溃只会留下不完整的临时文件，正式文件始终是最近一次完整提交。
type RetainStore struct {
	path string
}

func NewRetainStore(path string) *RetainStore {
	return &RetainStore{path: path}
}

func (s *RetainStore) Path() string { return s.path }

// Commit 原子写入一次完整快照。
func (s *RetainStore) Commit(rec RetainRecord) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("创建保持数据目录失败: %w", err)
	}
	payload := encodeRecord(rec)
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("打开保持数据临时文件失败: %w", err)
	}
	written := 0
	writeAll := func(b []byte) error {
		for len(b) > 0 {
			n, err := f.Write(b)
			if err != nil {
				return err
			}
			written += n
			b = b[n:]
		}
		return nil
	}
	werr := func() error {
		if err := writeAll(payload); err != nil {
			return err
		}
		return f.Sync()
	}()
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("写入保持数据失败（正式文件未受影响）: %w", werr)
	}
	_ = written
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("原子提交保持数据失败: %w", err)
	}
	// 目录 fsync，保证 rename 在掉电后仍可见（尽力而为，不影响正确性判定）。
	if dir, err := os.Open(filepath.Dir(s.path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

// Load 读取并严格校验快照；任何损坏、版本不符、布局不符都明确拒绝。
func (s *RetainStore) Load(programHash, layoutHash [32]byte) (RetainRecord, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return RetainRecord{}, err
		}
		return RetainRecord{}, fmt.Errorf("读取保持数据失败: %w", err)
	}
	rec, perr := decodeRecord(raw)
	if perr != nil {
		return RetainRecord{}, &RetainMismatchError{Kind: "Corrupt", Detail: perr.Error()}
	}
	if rec.ProgramHash != programHash {
		return RetainRecord{}, &RetainMismatchError{
			Kind:   "ProgramChanged",
			Detail: "保持数据来自不同程序版本（源程序指纹不一致），拒绝错位加载",
		}
	}
	if rec.LayoutHash != layoutHash {
		return RetainRecord{}, &RetainMismatchError{
			Kind:   "LayoutChanged",
			Detail: "保持项集合或顺序已变化，拒绝错位加载",
		}
	}
	return rec, nil
}

// RetainRecord 是一次保持快照的完整内容。
type RetainRecord struct {
	ProgramHash [32]byte
	LayoutHash  [32]byte
	Sequence    uint64
	BitValues   map[string]byte // 位项：0/1
	WordValues  map[string]int64
}

// Snapshot 从数据表中按布局抽取保持项。
func Snapshot(d *DataTable, programHash, layoutHash [32]byte, layout RetainLayout, seq uint64) (RetainRecord, error) {
	rec := RetainRecord{
		ProgramHash: programHash,
		LayoutHash:  layoutHash,
		Sequence:    seq,
		BitValues:   map[string]byte{},
		WordValues:  map[string]int64{},
	}
	for _, e := range layout.Entries {
		op := Operand{Kind: e.Kind, Offset: e.Offset, Size: e.Size}
		switch e.Kind {
		case OpMemory, OpOutput:
			if e.Size == 1 {
				op.Type = TypeBit
				v, err := d.readBit(op)
				if err != nil {
					return rec, err
				}
				if v {
					rec.BitValues[e.Name] = 1
				} else {
					rec.BitValues[e.Name] = 0
				}
			} else {
				op.Type = TypeWord
				v, err := d.readWord(op)
				if err != nil {
					return rec, err
				}
				rec.WordValues[e.Name] = v
			}
		case OpCounter:
			rec.WordValues[e.Name] = d.Counters[e.Offset].CV
		}
	}
	return rec, nil
}

// Restore 仅把保持项写回数据表，其他状态保持冷态。
func Restore(d *DataTable, layout RetainLayout, rec RetainRecord) error {
	for _, e := range layout.Entries {
		op := Operand{Kind: e.Kind, Offset: e.Offset, Size: e.Size}
		switch e.Kind {
		case OpMemory, OpOutput:
			if e.Size == 1 {
				op.Type = TypeBit
				b, ok := rec.BitValues[e.Name]
				if !ok {
					return &RetainMismatchError{Kind: "Corrupt", Detail: "缺少保持位 " + e.Name}
				}
				if err := d.writeBit(op, b != 0); err != nil {
					return err
				}
			} else {
				op.Type = TypeWord
				v, ok := rec.WordValues[e.Name]
				if !ok {
					return &RetainMismatchError{Kind: "Corrupt", Detail: "缺少保持字 " + e.Name}
				}
				if err := d.writeWord(op, v); err != nil {
					return err
				}
			}
		case OpCounter:
			v, ok := rec.WordValues[e.Name]
			if !ok {
				return &RetainMismatchError{Kind: "Corrupt", Detail: "缺少保持计数器 " + e.Name}
			}
			d.Counters[e.Offset].CV = v
			pv := d.Counters[e.Offset].PV
			d.Counters[e.Offset].Q = pv > 0 && v >= pv
		}
	}
	return nil
}

// 文件布局：magic(8) | programHash(32) | layoutHash(32) | seq(8) |
//
//	bitCount(4) | [lenName(2)+name+byte]... |
//	wordCount(4) | [lenName(2)+name+int64]... | sha256(全部前述字节)(32)
func encodeRecord(rec RetainRecord) []byte {
	buf := make([]byte, 0, 256)
	buf = append(buf, []byte(retainMagic)...)
	buf = append(buf, rec.ProgramHash[:]...)
	buf = append(buf, rec.LayoutHash[:]...)
	buf = binary.LittleEndian.AppendUint64(buf, rec.Sequence)

	bitNames := make([]string, 0, len(rec.BitValues))
	for n := range rec.BitValues {
		bitNames = append(bitNames, n)
	}
	sortStrings(bitNames)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(bitNames)))
	for _, n := range bitNames {
		buf = putName(buf, n)
		buf = append(buf, rec.BitValues[n])
	}
	wordNames := make([]string, 0, len(rec.WordValues))
	for n := range rec.WordValues {
		wordNames = append(wordNames, n)
	}
	sortStrings(wordNames)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(wordNames)))
	for _, n := range wordNames {
		buf = putName(buf, n)
		buf = binary.LittleEndian.AppendUint64(buf, uint64(rec.WordValues[n]))
	}
	sum := sha256.Sum256(buf)
	buf = append(buf, sum[:]...)
	return buf
}

func putName(buf []byte, name string) []byte {
	buf = binary.LittleEndian.AppendUint16(buf, uint16(len(name)))
	buf = append(buf, []byte(name)...)
	return buf
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

func decodeRecord(raw []byte) (RetainRecord, error) {
	var rec RetainRecord
	if len(raw) < len(retainMagic)+32+32+8+4+32 {
		return rec, errors.New("保持数据长度不足")
	}
	if string(raw[:len(retainMagic)]) != retainMagic {
		return rec, errors.New("魔数不匹配，不是本系统的保持数据")
	}
	bodyEnd := len(raw) - 32
	body, trailer := raw[:bodyEnd], raw[bodyEnd:]
	sum := sha256.Sum256(body)
	if string(sum[:]) != string(trailer) {
		return rec, errors.New("校验和不一致（写入可能中断或文件被损坏）")
	}
	pos := len(retainMagic)
	copy(rec.ProgramHash[:], raw[pos:pos+32])
	pos += 32
	copy(rec.LayoutHash[:], raw[pos:pos+32])
	pos += 32
	rec.Sequence = binary.LittleEndian.Uint64(raw[pos : pos+8])
	pos += 8

	readName := func() (string, error) {
		if pos+2 > bodyEnd {
			return "", errors.New("名称长度截断")
		}
		n := int(binary.LittleEndian.Uint16(raw[pos : pos+2]))
		pos += 2
		if pos+n > bodyEnd {
			return "", errors.New("名称内容截断")
		}
		name := string(raw[pos : pos+n])
		pos += n
		return name, nil
	}

	rec.BitValues = map[string]byte{}
	nb := int(binary.LittleEndian.Uint32(raw[pos : pos+4]))
	pos += 4
	for i := 0; i < nb; i++ {
		name, err := readName()
		if err != nil {
			return rec, err
		}
		if pos+1 > bodyEnd {
			return rec, errors.New("位值截断")
		}
		rec.BitValues[name] = raw[pos]
		pos++
	}
	rec.WordValues = map[string]int64{}
	nw := int(binary.LittleEndian.Uint32(raw[pos : pos+4]))
	pos += 4
	for i := 0; i < nw; i++ {
		name, err := readName()
		if err != nil {
			return rec, err
		}
		if pos+8 > bodyEnd {
			return rec, errors.New("字值截断")
		}
		rec.WordValues[name] = int64(binary.LittleEndian.Uint64(raw[pos : pos+8]))
		pos += 8
	}
	if pos != bodyEnd {
		return rec, errors.New("保持数据存在多余字节，结构对不上")
	}
	return rec, nil
}
