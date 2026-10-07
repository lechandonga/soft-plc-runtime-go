package plc

import (
	"errors"
	"fmt"
	"os"
)

// ReloadResult 说明热重载结果。
type ReloadResult struct {
	Applied bool
	Reason  string // ValidationRejected | RetainIncompatible
	Detail  string
}

// Reload 在扫描周期边界原子替换控制程序。
// 语义：新程序先在临时对象上完成全部校验与保持兼容判断——
//   - 任何一步失败：旧程序、旧状态、旧输出原样继续运行，返回结果 Applied=false；
//   - 全部通过：在持锁状态下整体切换，绝不出现新旧指令混执行。
//
// 保持规则：新程序若启用保持文件且文件已存在，程序指纹或布局不一致即拒绝
// （调用方可先显式删除保持文件做冷启动，或变更文件路径）。
func (r *Runtime) Reload(source string, cfg Config) ReloadResult {
	r.mu.Lock()
	defer r.mu.Unlock()

	cfg2, err := cfg.validated()
	if err != nil {
		return ReloadResult{Applied: false, Reason: "ValidationRejected", Detail: err.Error()}
	}
	prog, err := Parse(source)
	if err != nil {
		return ReloadResult{Applied: false, Reason: "ValidationRejected", Detail: err.Error()}
	}
	layout, err := prog.BuildRetainLayout(cfg2.Retain)
	if err != nil {
		return ReloadResult{Applied: false, Reason: "ValidationRejected", Detail: err.Error()}
	}
	if err := validateSafeOutputs(prog, cfg2.SafeOutputs); err != nil {
		return ReloadResult{Applied: false, Reason: "ValidationRejected", Detail: err.Error()}
	}
	if err := validateFaultInjection(prog, cfg2.FaultInjection); err != nil {
		return ReloadResult{Applied: false, Reason: "ValidationRejected", Detail: err.Error()}
	}

	var store *RetainStore
	if cfg2.RetainFile != "" {
		store = NewRetainStore(cfg2.RetainFile)
		if _, lerr := store.Load(prog.Hash, layout.Hash()); lerr != nil {
			if errors.Is(lerr, os.ErrNotExist) {
				// 新程序尚无保持文件：以当前已提交状态做首次完整提交。
				// 仅当新旧程序引用的保持地址在新数据表中仍可承载时允许冷建立。
			} else {
				return ReloadResult{Applied: false, Reason: "RetainIncompatible", Detail: lerr.Error()}
			}
		}
	}

	// 全部校验通过后，构造新数据表：保留可保持项，其余状态进入冷态。
	nd := newDataTable(prog)
	transferRetainedForReload(nd, prog, layout, r.committed, r.prog, r.layout)

	oldProg, oldCfg, oldLayout, oldData, oldCommitted, oldStore :=
		r.prog, r.cfg, r.layout, r.data, r.committed, r.store
	r.prog, r.cfg, r.layout, r.data, r.committed, r.store = prog, cfg2, layout, nd, nd.Clone(), store

	// 切换后立即确保输出整包一致：先按新映像下发一次，外部不会看到混合状态。
	if err := r.io.Write(r.data.Out); err != nil {
		// 下发失败：整体回滚到旧程序继续运行（保持旧故障处理路径由下次 Scan 判定）。
		r.prog, r.cfg, r.layout, r.data, r.committed, r.store =
			oldProg, oldCfg, oldLayout, oldData, oldCommitted, oldStore
		return ReloadResult{Applied: false, Reason: "ValidationRejected",
			Detail: fmt.Sprintf("新程序输出同步失败，已沿用旧程序: %v", err)}
	}

	// 若新程序启用保持但文件尚不存在，立即写入一份完整基线，保证掉电语义自始成立。
	if store != nil {
		if _, lerr := store.Load(prog.Hash, layout.Hash()); lerr != nil {
			rec, err := Snapshot(r.data, prog.Hash, layout.Hash(), layout, r.scanCount)
			if err == nil {
				err = store.Commit(rec)
			}
			if err != nil {
				r.prog, r.cfg, r.layout, r.data, r.committed, r.store =
					oldProg, oldCfg, oldLayout, oldData, oldCommitted, oldStore
				return ReloadResult{Applied: false, Reason: "RetainIncompatible",
					Detail: fmt.Sprintf("新程序保持基线写入失败，已沿用旧程序: %v", err)}
			}
		}
	}
	return ReloadResult{Applied: true}
}

// transferRetainedForReload 仅搬运“新旧程序中同名同布局”的保持项。
// 调用 Reload 的兼容前提是保持文件指纹/布局匹配；无保持文件时这里做尽力同名迁移，
// 非保持状态（定时器内部、边沿历史、计数器瞬态）一律冷态，杜绝错位。
func transferRetainedForReload(dst *DataTable, dstProg *Program, dstLayout RetainLayout,
	src *DataTable, srcProg *Program, srcLayout RetainLayout) {
	srcByName := map[string]RetainEntry{}
	for _, e := range srcLayout.Entries {
		srcByName[e.Name] = e
	}
	for _, e := range dstLayout.Entries {
		old, ok := srcByName[e.Name]
		if !ok || old.Kind != e.Kind || old.Offset != e.Offset || old.Size != e.Size {
			continue
		}
		op := Operand{Kind: e.Kind, Offset: e.Offset, Size: e.Size}
		switch e.Kind {
		case OpMemory, OpOutput:
			if e.Size == 1 {
				op.Type = TypeBit
				if v, err := src.readBit(op); err == nil {
					_ = dst.writeBit(op, v)
				}
			} else {
				op.Type = TypeWord
				if v, err := src.readWord(op); err == nil {
					_ = dst.writeWord(op, v)
				}
			}
		case OpCounter:
			if e.Offset < len(src.Counters) {
				dst.Counters[e.Offset].CV = src.Counters[e.Offset].CV
			}
		}
	}
}
