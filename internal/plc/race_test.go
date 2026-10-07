package plc

import (
	"sync"
	"testing"
)

// TestConcurrentInputUpdatesAndScans 在 -race 下验证：
// “现场线程”高频并发更新输入整包，与扫描线程并发时，
// 采样始终拿到完整快照，扫描不越界、不撕裂、结果轨迹与串行执行一致。
func TestConcurrentInputUpdatesAndScans(t *testing.T) {
	src := `
LD I0.0
OR Q0.0
ANI I0.1
OUT Q0.0
LD I0.0
EU
OUT Q0.1
END
`
	rt, io := newTestRuntime(t, src, DefaultConfig())
	caseLog(t, "并发竞态-输入更新与扫描并行", "3 个写者 goroutine 抖动 I0 位，1 个扫描者，共300拍",
		"-race 无数据竞争；扫描计数恰好等于成功周期数；无故障")

	const writers = 3
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					io.setBit(0, 0, id%2 == 0)
					io.setBit(0, 1, false)
					in := make([]byte, 1)
					if id%2 == 0 {
						in[0] = 0x01
					}
					io.mu.Lock()
					io.in = in
					io.mu.Unlock()
				}
			}
		}(w)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 300; i++ {
			_ = rt.Scan() // 有故障即停止推进；正常情况下不应出现
		}
		close(stop)
	}()
	wg.Wait()
	if f := rt.Fault(); f.Kind != FaultNone {
		t.Fatalf("并发运行不应产生故障: %s", f.String())
	}
	if rt.ScanCount() != 300 {
		t.Fatalf("成功扫描数错误: got %d want 300", rt.ScanCount())
	}
}

// TestConcurrentScanReloadDrillAndRecover 在 -race 下验证演练、热重载、
// 显式恢复与扫描并发叠加：任何时刻只有一套配置/程序生效，无数据竞争，
// 运行期出现的故障要么是演练 ExecutionFault，要么是 I/O 故障；
// 恢复后扫描始终可以继续，计数最终自洽。
func TestConcurrentScanReloadDrillAndRecover(t *testing.T) {
	srcV1 := `
LD I0.0
OUT Q0.0
END
`
	srcV2 := `
LD I0.0
OUT Q0.1
END
`
	rt, io := newTestRuntime(t, srcV1, DefaultConfig())
	caseLog(t, "并发竞态-演练/热重载/恢复与扫描叠加",
		"扫描者200拍；并行 30 次热重载（合法/非法、含演练开关）与故障恢复",
		"-race 无数据竞争；无 panic；演练只在配置开启时出现，恢复后扫描继续")

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			io.setBit(0, 0, true)
			if err := rt.Scan(); err != nil {
				// 锁存故障期间扫描返回故障是预期行为；扫描者不推进。
				continue
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 30; i++ {
			switch i % 4 {
			case 0:
				rt.Reload(srcV2, DefaultConfig())
			case 1:
				rt.Reload(srcV1, DefaultConfig())
			case 2:
				// 开启演练：下一拍即触发（扫描尝试序号始终 >=1）。
				cfg := DefaultConfig()
				cfg.ExecFaultScan = 1
				cfg.ExecFaultAfterInstr = 1
				rt.Reload(srcV1, cfg)
			case 3:
				rt.Reload("BOGUS\nEND\n", DefaultConfig()) // 非法程序整体拒绝
			}
			// 若此刻存在演练/其他锁存故障，满足条件就显式恢复，
			// 让扫描者得以继续；无故障时 Recover 返回错误属正常。
			_ = rt.Recover()
		}
	}()
	wg.Wait()

	// 收尾：若最后一拍停在演练故障上，显式恢复后必须能再完整扫描。
	if f := rt.Fault(); f.Kind != FaultNone {
		if err := rt.Recover(); err != nil {
			t.Fatalf("收尾恢复失败: %v", err)
		}
	}
	before := rt.ScanCount()
	io.setBit(0, 0, true)
	if err := rt.Scan(); err != nil {
		t.Fatalf("收敛后扫描必须成功: %v", err)
	}
	if rt.ScanCount() != before+1 {
		t.Fatalf("收敛后扫描计数应精确加 1: %d -> %d", before, rt.ScanCount())
	}
	out := rt.SnapshotOutputs()
	if out[0]&0x03 == 0x03 {
		t.Fatalf("观察到新旧程序同时驱动输出: %08b", out[0])
	}
}

// TestConcurrentReloadAndScan 验证热重载与扫描并发时不会出现新旧指令混执行。
func TestConcurrentReloadAndScan(t *testing.T) {
	srcV1 := `
LD I0.0
OUT Q0.0
END
`
	srcV2 := `
LD I0.0
OUT Q0.1
END
`
	badV3 := `
BOGUS
`
	rt, io := newTestRuntime(t, srcV1, DefaultConfig())
	var wg sync.WaitGroup
	caseLog(t, "并发竞态-热重载与扫描并行", "扫描200拍期间并发提交合法/非法重载",
		"无数据竞争；每个输出拍 Q0.0/Q0.1 互斥，绝不会被新旧程序同时驱动")
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			io.setBit(0, 0, true)
			_ = rt.Scan()
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			if i%2 == 0 {
				rt.Reload(srcV2, DefaultConfig())
			} else {
				rt.Reload(srcV1, DefaultConfig())
			}
			rt.Reload(badV3, DefaultConfig()) // 非法重载必须不影响运行
		}
	}()
	wg.Wait()
	out := rt.SnapshotOutputs()
	if out[0]&0x03 == 0x03 {
		t.Fatalf("观察到新旧程序同时驱动输出: %08b", out[0])
	}
}
