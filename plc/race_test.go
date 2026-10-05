package plc

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConcurrentIOAndScansUnderRace(t *testing.T) {
	caseLogger(t, "并发-I/O 更新与扫描竞态(go test -race)",
		"3 个现场线程随机翻转 X0/X1/X2，扫描线程持续跑 200ms，另一线程持续读输出",
		"无数据竞争、无 panic、结果始终取自采样快照、故障仅允许在人为注入时出现")

	src := "LD X0\nOR M0\nANI X1\nOUT M0\nLD M0\nOUT Y0\n" +
		"LD X2\nCTU C0 K100000\nLD C0\nOUT Y1\nEND\n"
	cfg := testConfig(t, "C0")
	cfg.CycleIntervalMillis = 1
	cfg.WatchdogMillis = 50
	cfg.IOSampleTimeoutMillis = 50
	rt, io := newRuntime(t, src, cfg)

	var stop atomic.Bool
	var wg sync.WaitGroup

	// 扫描线程。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			rt.ScanOnce()
		}
	}()

	// 现场抖动线程。
	for _, x := range []string{"X0", "X1", "X2"} {
		x := x
		wg.Add(1)
		go func() {
			defer wg.Done()
			v := false
			for !stop.Load() {
				v = !v
				io.SetInput(refOf(x), v)
				time.Sleep(time.Microsecond)
			}
		}()
	}

	// 观察者线程。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			_ = io.LastOutput(refOf("Y0"))
			_ = io.LastOutput(refOf("Y1"))
			_ = rt.Cycle()
			_ = rt.Fault()
			time.Sleep(time.Microsecond)
		}
	}()

	time.Sleep(200 * time.Millisecond)
	stop.Store(true)
	wg.Wait()

	if f := rt.Fault(); f != nil {
		t.Fatalf("无注入条件下不应出现故障: %v", f)
	}
	if rt.Cycle() == 0 {
		t.Fatal("应完成至少一个扫描周期")
	}
}

func TestDeterministicUnderConcurrentInput(t *testing.T) {
	caseLogger(t, "并发-同样输入快照下扫描结果与单线程一致",
		"在并发抖动环境中，通过在采样后固定快照的方式比对纯函数结果",
		"给定相同输入快照 map，连续两次内部执行必须得到完全相同输出")

	src := "LD X0\nAND X1\nORI X2\nOUT Y0\nLDP X0\nOUT Y1\nEND\n"
	cfg := testConfig(t)
	rt, _ := newRuntime(t, src, cfg)

	snap := map[Ref]bool{refOf("X0"): true, refOf("X1"): true, refOf("X2"): false}
	out1, err := rt.execute(1, snap)
	if err != nil {
		t.Fatal(err)
	}
	// 第二次用同一快照，边沿状态未提交（无 commitWork），结果必须一致。
	rt.pending = nil
	out2, err := rt.execute(1, snap)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range out1 {
		if out2[k] != v {
			t.Fatalf("同快照结果不确定: %s %v vs %v", k, v, out2[k])
		}
	}
}
