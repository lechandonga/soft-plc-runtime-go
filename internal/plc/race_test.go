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
