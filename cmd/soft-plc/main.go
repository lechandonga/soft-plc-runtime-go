// Command soft-plc 在本地运行一个完全模拟的软 PLC：
// 程序文件 + JSON 配置 + 输入脚本（逐拍 I 区整包），无任何网络或硬件依赖。
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/lechandonga/soft-plc-runtime-go/internal/plc"
	"github.com/lechandonga/soft-plc-runtime-go/internal/simio"
)

func main() {
	var (
		programPath = flag.String("program", "testdata/programs/example.txt", "控制程序文件")
		configPath  = flag.String("config", "testdata/programs/config.json", "JSON 配置文件")
		scriptPath  = flag.String("script", "testdata/scripts/inputs.txt", "逐拍输入脚本（每周期一行，空行保持上一拍）")
		cycles      = flag.Int("cycles", 20, "运行的扫描周期数；<=0 表示按脚本长度")
		verbose     = flag.Bool("v", false, "打印每周期采样/执行/提交追踪")
	)
	flag.Parse()

	if err := run(*programPath, *configPath, *scriptPath, *cycles, *verbose); err != nil {
		fmt.Fprintln(os.Stderr, "soft-plc:", err)
		os.Exit(1)
	}
}

func run(programPath, configPath, scriptPath string, cycles int, verbose bool) error {
	src, err := os.ReadFile(programPath)
	if err != nil {
		return fmt.Errorf("读取程序失败: %w", err)
	}
	cfg, err := plc.LoadConfigFile(configPath)
	if err != nil {
		return err
	}

	// 先用一个临时解析得到输入/输出尺寸（正式校验在 NewRuntime 里完成）。
	probe, err := plc.Parse(string(src))
	if err != nil {
		return err
	}
	mod := simio.NewSimModule(probe.InputSize, probe.OutputSize)

	rt, err := plc.NewRuntime(string(src), cfg, mod)
	if err != nil {
		return err
	}
	if verbose {
		rt.Trace = func(scan uint64, phase string) {
			fmt.Printf("#%d %s\n", scan, phase)
		}
	}

	frames, err := loadInputScript(scriptPath)
	if err != nil {
		return err
	}
	if cycles <= 0 {
		cycles = len(frames)
	}

	for i := 0; i < cycles; i++ {
		if i < len(frames) {
			mod.LoadInputs(frames[i])
		}
		if err := rt.Scan(); err != nil {
			f := rt.Fault()
			return fmt.Errorf("第 %d 周期进入故障: %s（输出已落安全值，需显式恢复）", i+1, f.String())
		}
		out := rt.SnapshotOutputs()
		fmt.Printf("scan %03d: in=%s out=%s\n", i+1, fmtBytes(mod.Inputs()), fmtBytes(out))
	}
	fmt.Printf("完成 %d 个周期，扫描计数=%d\n", cycles, rt.ScanCount())
	return nil
}

func fmtBytes(b []byte) string {
	parts := make([]string, len(b))
	for i, v := range b {
		parts[i] = fmt.Sprintf("%02X", v)
	}
	return strings.Join(parts, " ")
}

// loadInputScript 解析逐拍输入：每行形如 "00 01"（十六进制字节，覆盖整包 I 区）。
// 以 '#' 开头为注释；空行表示该拍保持上一拍输入（返回 nil 帧，由调用方跳过装入）。
func loadInputScript(path string) ([][]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取输入脚本失败: %w", err)
	}
	var frames [][]byte
	for lineNo, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		tokens := strings.Fields(line)
		frame := make([]byte, 0, len(tokens))
		for _, t := range tokens {
			var v byte
			if _, err := fmt.Sscanf(t, "%x", &v); err != nil {
				return nil, fmt.Errorf("输入脚本第%d行非法字节 %q", lineNo+1, t)
			}
			frame = append(frame, v)
		}
		frames = append(frames, frame)
	}
	return frames, nil
}
