package plc

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestE2EBinaryRetainAcrossProcesses 构建 CLI 二进制，运行两次独立进程，
// 第二次进程必须读到第一次进程的保持文件（验证真实“掉电重启”场景）。
func TestE2EBinaryRetainAcrossProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("短模式跳过二进制集成测试")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "soft-plc")
	build := exec.Command("go", "build", "-o", bin, "../../cmd/soft-plc")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("构建 CLI 失败: %v\n%s", err, out)
	}

	work := t.TempDir()
	prog := filepath.Join(work, "p.txt")
	cfgPath := filepath.Join(work, "c.json")
	script := filepath.Join(work, "in.txt")
	retain := filepath.Join(work, "retain.bin")

	// 程序：I0.0 上升沿计数到 10 点亮 Q0.0；计数器保持。
	program := `
LD I0.0
CTU C0 10 I0.0 I0.1
LD C0.Q
OUT Q0.0
END
`
	os.WriteFile(prog, []byte(program), 0o644)
	config := `{
 "scan_period_ms": 1,
 "watchdog_instructions": 100000,
 "retain": ["C0"],
 "retain_file": "` + retain + `",
 "safe_outputs": {"Q0.0": 0}
}`
	os.WriteFile(cfgPath, []byte(config), 0o644)
	// 6 个完整按下/松开脉冲 = CV 累积 6（不到 10，Q0.0 保持灭）
	os.WriteFile(script, []byte(strings.Repeat("01\n00\n", 6)), 0o644)

	run1 := exec.Command(bin, "-program", prog, "-config", cfgPath, "-script", script, "-cycles", "12")
	out1, err := run1.CombinedOutput()
	if err != nil {
		t.Fatalf("第一次进程失败: %v\n%s", err, out1)
	}
	caseLog(t, "端到端-第一次进程完整提交6次计数", "12 周期（6 个脉冲）",
		"保持文件生成；输出 Q0.0 始终为 0")
	if strings.Contains(string(out1), "out=01") {
		t.Fatalf("未到 PV 不应点亮: %s", out1)
	}
	if _, err := os.Stat(retain); err != nil {
		t.Fatalf("保持文件应已生成: %v", err)
	}

	// 第二个进程：再给 6 个脉冲，CV 从 6 继续，第 4 个脉冲到达 PV=10，Q0.0 点亮
	run2 := exec.Command(bin, "-program", prog, "-config", cfgPath, "-script", script, "-cycles", "12")
	out2, err := run2.CombinedOutput()
	if err != nil {
		t.Fatalf("第二次进程失败: %v\n%s", err, out2)
	}
	caseLog(t, "端到端-新进程恢复 C0=6 后继续计数", "同样脚本再跑12周期",
		"第 8 个扫描周期（第4个脉冲）起 Q0.0=1")
	lines := strings.Split(strings.TrimSpace(string(out2)), "\n")
	var litAt string
	for _, ln := range lines {
		if strings.Contains(ln, "out=01") {
			litAt = ln
			break
		}
	}
	if litAt == "" {
		t.Fatalf("跨进程累计未达到 PV: %s", out2)
	}
	t.Logf("点亮周期: %s", strings.TrimSpace(litAt))
}
