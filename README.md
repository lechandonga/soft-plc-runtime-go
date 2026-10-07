# soft-plc-runtime-go

纯 Go 标准库实现的本地软 PLC 运行时：用**本地模拟 I/O 模块**和**本地程序文件**代替真实控制器与现场设备，
覆盖程序加载校验、严格循环扫描、看门狗/故障恢复与掉电保持，所有结果确定、可复现、可解释。
不依赖外部服务、真实硬件、网络或任何第三方 Go 依赖。

## 目录结构

```
cmd/soft-plc/          本地 CLI（程序 + JSON 配置 + 逐拍输入脚本）
internal/plc/          解析校验、数据表、执行器、扫描运行时、热重载、保持存储
internal/simio/        内存模拟 I/O 模块（整包快照、断线/重连）
testdata/programs/     示例程序与配置
testdata/scripts/      逐拍输入脚本
docs/                  程序格式 / 扫描与故障语义 / 保持规则 / 配置 / 本地验证
```

## 快速开始

```bash
go run ./cmd/soft-plc -cycles 12 -v
go test ./... -count=1
CGO_ENABLED=1 go test ./... -race -count=1
```

## 文档

- `docs/program-format.md` — 程序文本格式、地址、指令集与启动前校验分类
- `docs/runtime-semantics.md` — 采样/执行/提交三阶段、看门狗、故障锁存与恢复、热重载原子性
- `docs/retain.md` — 保持项配置、版本/布局兼容判定、原子文件格式与重启恢复
- `docs/configuration.md` — JSON 可配置项与输入脚本格式
- `docs/local-verification.md` — 本地验证命令与测试场景清单

## 本轮新增能力

- **墙钟单周期保护**：`watchdog_time_ms`（毫秒，缺省 `0` 关闭）从输入采样前计时到输出提交完成，
  与指令条数预算相互独立、原因可区分；触发后当轮整体回滚、输出落安全值、故障锁存。
- **执行故障演练（仅本地，默认关闭）**：`exec_fault_scan`/`exec_fault_scan_end`/
  `exec_fault_after_instructions` 可在指定扫描周期确定性注入一次 `ExecutionFault`，
  完整演练回滚、安全输出、锁存与显式恢复链路。
- **写目标加载校验**：写入指令目标必须是真正可写的存储对象且对象种类匹配；
  定时器/计数器字段等运行时自维护状态量按新增分类 `IllegalTarget` 在加载期拒绝，
  冷启动与热重载口径一致。

三项能力缺省全部关闭或不改变既有分类，老配置文件不改即可用，不引入第三方依赖。
