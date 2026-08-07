# Code quality review（Issue #889）

- 日期：2026-08-07
- 分支：`chore/issue-889-code-quality`
- 范围：当前 checkout 的 `cmd/`、`internal/`、`tools/` Go 代码；仅移除逐个验证为真死码的函数，不做重构。
- Issue comments：`gh issue view 889 --comments` 返回无评论，因此没有额外 Agent 建议可合并。

## 方法与门禁

执行了：

```text
go install golang.org/x/tools/cmd/deadcode@latest
deadcode -filter '^github.com/miaoxiaoyong/sift/' ./...
go install honnef.co/go/tools/cmd/staticcheck@latest
staticcheck ./...
go test ./...
go vet ./...
git diff --check
```

复杂度以 `gocyclo -over 20 .` 作筛查；该数值是排序线索，不是功能缺陷判定。所有 deadcode 候选均用 `rg` 在生产 Go 源码和 `*_test.go` 中逐名确认，并检查了生成器、反射、CLI 及兼容性用途。

## P0

无。

## P1

无确认的 P1 非功能性问题。当前高复杂度代码主要影响可读性和后续变更风险，未发现本次应冒险重构的行为安全问题。

## P2 findings

### 1. 已移除的确认真死码

以下符号在主代码、测试、CLI 和工具源码中均无调用，且没有反射/生成器/兼容性注释或外部边界用途：

| 文件 | 符号 | 处置 |
|---|---|---|
| `internal/command/compile.go` | `SortedOptions` | 删除；同时删除仅供该函数使用的 `sort` 导入 |
| `internal/launchworker/launch.go` | `BackendRouter.Spawn` | 删除；Worker 已直接按冻结 backend 选择并调用 host |
| `internal/storage/events.go` | `CommandAckOperationKey` | 删除；实际使用的是 `internal/command.AckOperationKey` |
| `internal/storage/events.go` | `LabelsOperationKey` | 删除；仓库中无调用 |

这些删除是机械性的，无逻辑路径改变。

### 2. deadcode 候选但保留

`deadcode` 首轮报告 108 个函数，其中 36 个位于测试 fake 文件，按约束全部排除：

- `internal/attempt/fake.go`
- `internal/brain/fake.go`
- `internal/forge/fake.go`

其余候选中，以下类别已确认有调用或不可按静态调用图删除，全部保留：

- **测试/集成 fixture 使用**：`brain` 的 T2/TaskSpec/T7 结果辅助、`config` 的 drift/guard/probe 辅助、`daemon.AssembleWithRunner`/`Daemon.Tick`、`forge.NewGitHub`/`NewGitLab`、`gate.Cache.EvaluateCached`、`replay` 导出回放入口、`runtime` topology 入口、`skeleton.Chain`、`worktree.Manager` 方法等。`deadcode` 不把测试作为根，因此这些会被误报。
- **反射/生成器使用**：`internal/schema/contract.go` 的 `EnumValues` 和 `internal/schema/rules.go` 的字段规则 accessor 由 schema decoder 或 `tools/schema` 通过接口/反射使用，不能依据静态调用图删除。
- **明确兼容性/未来扩展 seam**：`runtime.PermitGate.SpawnOnce`、`storage.NewReconcilerScheduler`、`runtime.NewWorktreeManager` 等虽当前无仓库调用，但源码明确标注兼容性或 Runtime-facing API，删除会破坏潜在内部消费者；建议单独决策后再清理。
- **调用链内部函数**：如 `sameMillis`、`checkRepoDir`、`each`、`sameVerdict`、`observeTopology` 由同文件被报告函数间接使用，不能只按报告行删除。

### 3. 冗余逻辑

- `internal/forgeworker/` 下的 `CommentWorker`、`ChangeWorker`、`MergeWorker`、`AlertWorker`、`CommandAckWorker`、`GateReEvaluationWorker` 和 `RerunChecksWorker` 都重复“claim durable operation → decode/route → 执行副作用 → complete operation”的外壳。各 worker 的不确定结果和错误分类不同，当前不应直接合并成一个过度泛化的 worker；后续可只提取 claim/finish 的小型私有 helper，并逐个保留现有故障语义测试。
- operation-key 生成曾同时存在 `command.AckOperationKey` 与未使用的 `storage.CommandAckOperationKey`；后者已删除。其余 key builder 仍对应不同持久化边界，不建议为表面统一再抽象。
- `storage.NewReconcilerScheduler` 和 `runtime.NewWorktreeManager` 是新实现的薄转发/兼容层，详见下方过度设计候选；在兼容性确认前保留。

### 4. 高圈复杂度

`gocyclo -over 20 .` 的生产代码最高项包括：

- `storage.(*DB).insertInterruptEmissionTx`：77
- `storage.validateEffectBinding`：74
- `storage.(*DB).ResolveAttemptRace`：63
- `channelworker.WebhookAdapter.Publish`：60
- `wrapper.RunExecution`：59
- `config.normalizeAttention`：54
- `launchworker.(*Worker).RunOnce`：52
- `storage.(*DB).ApplyStartupRecoveryAction`：51
- `brain.BuildT7Input`：51
- `storage.applyChannelOutcomeTx`：48
- `storage.(*DB).AdvanceInterrupt`：46
- `gate.Validate`：45

建议后续按“输入/契约校验 → 快照/证据构造 → 单事务写入 → 结果映射”拆分，并为每个阶段保留现有 CAS/幂等测试；本 issue 不擅自重构这些安全敏感事务。

### 5. 过度设计/兼容层候选

- `storage.ReconcilerScheduler` 是 `OutboxScheduler` 的别名，`NewReconcilerScheduler` 只是转调新构造器。
- `runtime.WorktreeManager`/`NewWorktreeManager` 是 `worktree` 包的薄别名/转发层，但生产 daemon 直接使用 `worktree.NewManager`。
- `runtime.PermitGate.SpawnOnce` 是 `StartOnce` 的 error-only 包装，当前无调用。

建议确认是否仍需支持旧内部调用者；若确认没有，再单独开变更删除兼容层，而不是在本次质量 review 中混入 API 收缩。

### 6. 单生产实现接口（保留为测试/平台 seam）

以下接口当前各只有一个生产实现，但有明确注入价值，不建议仅为减少接口数量而改动：

- `runtime.Launcher` → `DirectLauncher`（注释明确为未来 sandbox seam）。
- `channelworker.SecretResolver` → `EnvironmentSecretResolver`、`WebhookSender` → `HTTPWebhookSender`（测试替身和凭据边界）。
- `cmd/siftd` 的 `schedulerFactory`/`wakeScheduler`（生产 scheduler 与有界测试替身）。
- `forgeworker.RerunCheckClient`（按 forge 路由注入，测试可避免真实 CLI）。

## 建议优先级汇总

| 优先级 | 数量 | 建议 |
|---|---:|---|
| P0 | 0 | 无 |
| P1 | 0 | 暂无阻断性质量问题 |
| P2 | 4 已清理 + 兼容/复杂度观察项 | 继续以单独 issue 拆分高复杂度事务；另行决定兼容层生命周期 |

## 验收结果

- [x] deadcode 扫描并逐名 grep 验证
- [x] 未删除测试 fake、反射/生成器使用或 CLI/兼容性候选
- [x] 删除 4 个确认真死函数
- [x] findings 报告按 P0/P1/P2 给出可执行建议
- [x] `go test ./...`
- [x] `go vet ./...`
- [x] `staticcheck ./...`（staticcheck 2026.1 / v0.7.0，无输出）
- [x] `git diff --check`

剩余风险：deadcode 对测试根、反射和未来兼容 API 的判断天然不完备；保留项需要在 API 生命周期或调用方边界明确后再处理。高复杂度事务仍有维护风险，但本次未改变其行为。
