// 本地软 PLC 运行时命令行。
//
// 用法：
//
//	go run ./cmd/plc -program examples/conveyor.plc -config examples/config.json -cycles 800
//
// 全部 I/O 来自内置的内存模拟模块：X0 启动、X1 停止、X2 工件脉冲，
// 不访问网络、不依赖外部服务与硬件。
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lechandonga/soft-plc-runtime-go/plc"
)

func main() {
	programPath := flag.String("program", "examples/conveyor.plc", "控制程序文件")
	configPath := flag.String("config", "examples/config.json", "运行时配置文件")
	cycles := flag.Int("cycles", 0, "演示扫描周期数；0 表示持续运行到 Ctrl-C")
	flag.Parse()

	prog, err := plc.LoadProgramFile(*programPath)
	if err != nil {
		fail("程序加载被拒绝", err)
	}
	cfg, err := plc.LoadConfig(*configPath)
	if err != nil {
		fail("配置校验失败", err)
	}

	io := plc.NewSimIO()
	rt, err := plc.NewRuntime(prog, cfg, io, logf)
	if err != nil {
		fail("运行时启动失败（保持数据不兼容时属确定性拒绝）", err)
	}

	stopSig := make(chan os.Signal, 1)
	signal.Notify(stopSig, syscall.SIGINT, syscall.SIGTERM)

	go rt.Run()

	// 确定性现场脚本：周期 20 按下启动，之后每 50 个周期来一个工件脉冲。
	go func() {
		n := 0
		ticker := time.NewTicker(time.Duration(cfg.CycleIntervalMillis) * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			n++
			if n == 20 {
				io.SetInput(mustRef("X0"), true)
			}
			if n == 22 {
				io.SetInput(mustRef("X0"), false)
			}
			if n >= 30 && n%50 == 0 {
				io.SetInput(mustRef("X2"), true)
			}
			if n >= 30 && n%50 == 1 {
				io.SetInput(mustRef("X2"), false)
			}
			if *cycles > 0 && n >= *cycles {
				rt.Stop()
				return
			}
		}
	}()

	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if f := rt.Fault(); f != nil {
				logf("当前故障: %v", f)
				return
			}
			logf("周期 %d Y0=%v Y2=%v Y3=%v",
				rt.Cycle(), io.LastOutput(mustRef("Y0")),
				io.LastOutput(mustRef("Y2")), io.LastOutput(mustRef("Y3")))
		}
	}()

	if *cycles > 0 {
		time.Sleep(time.Duration(*cycles+2) * time.Duration(cfg.CycleIntervalMillis) * time.Millisecond)
	} else {
		<-stopSig
	}

	rt.Stop()
	// 停机前完整提交一次保持数据（最近一次完整状态）。
	if err := rt.Persist(); err != nil {
		fail("保持数据提交失败", err)
	}
	logf("正常停机：保持数据已完整提交到 %s", cfg.StateFile)
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stdout, "[PLC] "+format+"\n", args...)
}

func fail(what string, err error) {
	fmt.Fprintf(os.Stderr, "[PLC] %s: %v\n", what, err)
	os.Exit(1)
}

func mustRef(s string) plc.Ref {
	r, err := plc.ParseAddress(s)
	if err != nil {
		panic(err)
	}
	return r
}
