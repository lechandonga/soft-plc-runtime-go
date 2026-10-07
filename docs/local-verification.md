# 本地验证方法

全程不需要网络、真实硬件或第三方依赖（仅用 Go 标准库；Go >= 1.25）。

## 一键跑全部测试

```bash
# 普通测试
go test ./... -count=1

# 竞态检测（需要 cgo/gcc）
CGO_ENABLED=1 go test ./... -race -count=1

go vet ./...
```

## 测试覆盖的业务场景

| 测试 | 覆盖点 |
| --- | --- |
| `TestParseAcceptsValidProgram` | 合法程序整体通过、区域尺寸推导 |
| `TestLoadRejections` | 语法错误、未知指令、非法地址、缺失引用、类型错误、结构错误等 15 类拒绝 |
| `TestRetainConfigValidation` | 保持项重复/悬空/非法区域拒绝 |
| `TestScanConsistency` | 相同输入序列两次运行逐拍一致；边沿只在上升沿出现 |
| `TestTimerCounterDeterminism` | TON 三周期到点与输入撤销复位；CTU 上升沿计数、到 PV、复位 |
| `TestWatchdogFaultAndRecovery` | 死循环超预算进入 WatchdogFault；锁存；显式恢复条件 |
| `TestWallClockDeadlineTriggers` | 墙钟上限在指令未超时下独立触发；时间原因；当轮回滚、安全值、锁存、显式恢复 |
| `TestWallClockDeadlineNoFalsePositive` | 正常模拟 I/O/执行开销不误报（虚拟时钟钉死开销 + 真实时钟连跑兜底） |
| `TestWallClockCoversSampleAndCommit` | 慢采样/慢提交也计入墙钟，真超必报 |
| `TestDeadlineVsInstructionBudget` | 两道保护仅条数/仅墙钟/同点到点/先后触发四种组合的原因区分 |
| `TestDrillFaultFullChain` | 演练触发 ExecutionFault；异常点前后半截处置；恢复后从最近提交继续 |
| `TestDrillFaultDistinguishable` | 演练故障与 I/O 失联、超时、保持写入失败种类可区分 |
| `TestDrillFiresExactlyOnce` | 演练窗口内只确定性触发一次，恢复后不二次注入 |
| `TestDrillDisabledByDefault` / `TestDrillConfigValidation` | 缺省关闭零影响；非法演练配置启动期拒绝 |
| `TestDrillRearmedAfterReload` | 热重载后演练按新配置重新装弹/关闭，非法配置整体拒绝 |
| `TestWriteTargetRejectionMatrix` | 22 组“写入指令 × 目标对象”组合的加载期分类拒绝 |
| `TestWriteTargetAcceptanceMatrix` | 合法写入组合（含 `RST C0`）放行且冷启动可执行 |
| `TestWriteTargetRejectedOnColdStartAndReload` | 非法写目标在冷启动与热重载两路径同口径拒绝 |
| `TestIOFaultSafeOutputsAndNoAutoRecovery` | 断线 IOFault、安全输出落 0、重连不自动恢复、恢复后续跑 |
| `TestCommitFailureRollsBackState` | 输出提交失败时周期内状态整体回滚 |
| `TestHotReloadAtomicity` | 非法新程序整体拒绝并沿用旧程序；合法程序整体切换 |
| `TestHotReloadRejectsRetainMismatch` | 保持布局变化时热重载拒绝 |
| `TestRetainRestartConsistency` | 多周期后“重启”CV 恢复；截断的半写文件判 Corrupt 拒绝 |
| `TestRetainRejectsProgramChange` | 程序版本变化时保持数据拒绝错位加载 |
| `TestConcurrentInputUpdatesAndScans` | I/O 更新与扫描并发，`-race` 下无竞争、计数精确 |
| `TestConcurrentReloadAndScan` | 热重载与扫描并发，无新旧指令混执行、输出不被双驱动 |
| `TestConcurrentScanReloadDrillAndRecover` | 演练、热重载、显式恢复与扫描并发叠加，`-race` 下稳定收敛 |
| `TestE2EBinaryRetainAcrossProcesses` | 真实构建 CLI，两个独立进程通过保持文件接力计数 |

每个用例都通过 `caseLog` 打印：覆盖的业务、具体输入、判定依据，`go test -v` 可直接审计。

## 手动运行演示程序

```bash
go run ./cmd/soft-plc \
  -program testdata/programs/example.txt \
  -config  testdata/programs/config.json \
  -script  testdata/scripts/inputs.txt \
  -cycles 12 -v
```

参数：`-program` 程序文件、`-config` JSON 配置、`-script` 逐拍输入、`-cycles` 周期数、`-v` 打印采样/执行/提交追踪。
演示程序包含自锁、TON、CTU、EU 边沿与保持字；保持文件落在 `testdata/run/retain.bin`，
删除该文件即可冷启动。

同一条验证命令连着跑两遍都应成功；演示产生的 `testdata/run/retain.bin` 属于运行残留
（已在 `.gitignore` 中忽略），冷启动验证前删除它即可，仓库本身不依赖任何上次运行留下的文件。

## 演练手段用法（仅本地）

墙钟保护：在配置中加 `"watchdog_time_ms": 20`（毫秒，`0` 或缺省关闭）。

执行故障演练：在配置中给出命中周期与注入位置，例如让第 3 个扫描周期在第 2 条指令执行后抛异常：

```json
{
  "exec_fault_scan": 3,
  "exec_fault_after_instructions": 2,
  "safe_outputs": {"Q0.0": 0}
}
```

要按周期段演练可用 `exec_fault_scan_end`（闭区间，窗口内第一个到达周期触发，只触发一次）。
演练只通过配置文件显式开启，定位仅限本地验证故障链路；正常运行不要配置这些字段。
