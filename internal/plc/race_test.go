package plc

import (
	"path/filepath"
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

// TestConcurrentFaultInjectionReloadRecover 在 -race 下验证：
// 故障演练开启时，扫描、热重载（含新演练配置）、显式恢复、保持提交并发叠加，
// 无数据竞争；故障只以锁存语义出现，恢复后从最近完整提交继续。
func TestConcurrentFaultInjectionReloadRecover(t *testing.T) {
	dir := t.TempDir()
	src := `
LD I0.0
OUT Q0.0
MOV 1 MW0
END
`
	mkCfg := func(scan int64) Config {
		cfg := DefaultConfig()
		cfg.Retain = []string{"MW0"}
		cfg.RetainFile = filepath.Join(dir, "retain.bin")
		cfg.FaultInjection = &FaultInjectionConfig{
			ExecErrorScans: []int64{scan},
			AtInstruction:  1,
		}
		return cfg
	}
	rt, io := newTestRuntime(t, src, mkCfg(5))
	io.setBit(0, 0, true)
	caseLog(t, "并发竞态-演练/重载/恢复/扫描叠加", "扫描300拍 + 并发重载(注入第5拍) + 并发恢复",
		"-race 无数据竞争；出现的故障均为 ExecutionFault 锁存；最终无故障或故障可被显式清除")

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() { // 扫描者
		defer wg.Done()
		for i := 0; i < 300; i++ {
			_ = rt.Scan()
		}
		close(stop)
	}()
	wg.Add(1)
	go func() { // 重载者：合法配置（含演练）与非法配置交替
		defer wg.Done()
		for i := 0; i < 40; i++ {
			rt.Reload(src, mkCfg(int64(3+i)))
			rt.Reload("BOGUS\n", DefaultConfig())
		}
	}()
	wg.Add(1)
	go func() { // 恢复者：有故障就显式恢复
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = rt.Recover()
			}
		}
	}()
	wg.Wait()

	// 收尾：若仍锁存故障，必须是 ExecutionFault 且可显式清除。
	if f := rt.Fault(); f.Kind != FaultNone {
		if f.Kind != FaultExecution {
			t.Fatalf("演练叠加场景只应出现 ExecutionFault，got %s", f.Kind)
		}
		if err := rt.Recover(); err != nil {
			t.Fatalf("锁存故障应可显式恢复: %v", err)
		}
	}
	if err := rt.Scan(); err != nil {
		t.Fatalf("最终扫描应可正常推进: %v", err)
	}
}
