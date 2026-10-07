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
| `TestScanTimeLimitTriggers` | 墙钟上限触发 ScanTimeoutFault、回滚、安全值、锁存、恢复基线（确定性假时钟） |
| `TestScanTimeLimitVsWatchdogDistinct` | 条数超限 → WatchdogFault、耗时超限 → ScanTimeoutFault，两道保护可区分 |
| `TestScanTimeLimitNoFalsePositiveWithSimIO` | 真实模拟 I/O 正常开销在合理上限下 200 拍零误报 |
| `TestScanTimeLimitDefaultDisabled` | 缺省 `scan_time_limit_ms=0` 关闭保护，兼容既有配置 |
| `TestScanTimeLimitHotReloadAtomic` | 墙钟上限随热重载原子生效；非法配置整体拒绝 |
| `TestFaultInjectionDrillFullChain` | 演练注入走完整 ExecutionFault 链路：回滚、安全值、锁存、恢复后继续 |
| `TestFaultInjectionDisabledNoEffect` | 演练关闭/未命中时对正常运行零影响 |
| `TestFaultInjectionConfigValidation` | 演练配置非法时冷启动与热重载一致拒绝 |
| `TestWriteTargetRejections` / `TestWriteTargetAccepted` | 写目标校验：11 种非法“指令×目标”按 IllegalWriteTarget 拒绝，合法目标不误伤 |
| `TestWriteTargetColdStartAndHotReload` | 写目标校验在冷启动与热重载两条路径口径一致 |
| `TestIOFaultSafeOutputsAndNoAutoRecovery` | 断线 IOFault、安全输出落 0、重连不自动恢复、恢复后续跑 |
| `TestCommitFailureRollsBackState` | 输出提交失败时周期内状态整体回滚 |
| `TestHotReloadAtomicity` | 非法新程序整体拒绝并沿用旧程序；合法程序整体切换 |
| `TestHotReloadRejectsRetainMismatch` | 保持布局变化时热重载拒绝 |
| `TestRetainRestartConsistency` | 多周期后“重启”CV 恢复；截断的半写文件判 Corrupt 拒绝 |
| `TestRetainRejectsProgramChange` | 程序版本变化时保持数据拒绝错位加载 |
| `TestConcurrentInputUpdatesAndScans` | I/O 更新与扫描并发，`-race` 下无竞争、计数精确 |
| `TestConcurrentReloadAndScan` | 热重载与扫描并发，无新旧指令混执行、输出不被双驱动 |
| `TestConcurrentFaultInjectionReloadRecover` | 演练/热重载/恢复/扫描并发叠加，`-race` 下稳定 |
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
