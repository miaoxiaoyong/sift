FAIL

# report.md 字段级评审

> 日期：2026-07-29
> 评审人：pi × GPT-5.6-sol
> 评审对象：[`docs/specs/report.md`](../specs/report.md) draft（#288 / PR #293，merge `d4b0d25`）
> 依据：[PRD §5.8、§9.1 TM5](../PRD.md)、[DESIGN §8.9](../DESIGN.md)、[WBS M5 §5.5](../WBS.md)、[ADR-006](../decisions/006-report-via-cli-not-mcp.md)、[ADR-008](../decisions/008-control-plane-endpoints-and-capabilities.md)
> 交叉核对：active [`control-plane.md`](../specs/control-plane.md) §3–§5/§7–§9、[`config.md`](../specs/config.md) §3.9–§3.10、[`storage.md`](../specs/storage.md) §5.2/§7.1/§7.4/§9/§11–§12，以及 draft [`interrupt.md`](../specs/interrupt.md) §1–§5/§8.3

## 1. 结论

**FAIL（5×P1）。** 候选稿正确冻结了四类 closed payload、`completed` 非证据边界、attempt 级鉴权、两层去重、持久整数令牌桶和 Report 直接致扰子配额；但 retry 配置无法到达 CLI，运行期配置快照来源未定，Report 子配额缺可落库的幂等收费身份，`failure_review_once` 与 Interrupt 唯一 generation preimage 冲突，且 blocker/Interrupt 失败时的事务结果互相矛盾。两个实现仍不能从现稿生成同一 RPC、预算记录和崩溃事务。

`docs/specs/report.md` 必须保持 `status: draft`。本结论不表示 Report、Attention 或 M5 已实现，也不回退已通过的 M4 门禁。

## 2. P1 阻断与可执行关闭条件

| 项 | 发现 | 为什么阻断 | 可执行关闭条件 |
|---|---|---|---|
| R1 | **`not_ready` 的配置化退避无法到达 CLI，且示例不是合法 Request v1。** report §4 要求 CLI 使用 `report.not_ready_initial_delay/max_delay/total_timeout` 与 `runtime.retry_multiplier`；但 CLI 按 §1–§2 只读 `control.json`，该文件没有这些字段，active control-plane §5.2 的错误 details 又明确只含 `retry_after_ms`。服务端也没有 retry ordinal，无法替无状态 CLI 推导后续指数项。report §2 所称“唯一请求”还缺 Request v1 必填的 protocol/client/request 字段。 | 自定义非默认配置下，CLI 只能偷读 `config.yaml`、硬编码默认值或无限相信单个 delay；三者分别违反通道边界、配置契约或总超时。照示例发送则会被 active control-plane closed envelope 拒绝。 | 在 report + control-plane 选择一条唯一 wire contract：例如每个 `not_ready` 返回从该 Run 冻结配置导出的 closed retry policy（首延迟、倍率、封顶、总时限），CLI 从首次 `not_ready` 起以单调时钟累计并计算；或把等价字段版本化写入 `control.json`。同步给完整 Request v1 示例、details schema、非法值/响应校验与边界 vectors。 |
| R2 | **所有 Report 参数使用哪个配置快照未冻结。** Run 已有创建时不可变的 `runs.config_snapshot_id`，但 report 对 payload 上限、dedupe window、令牌桶参数、retry 和子配额/`day_timezone` 均只写配置路径，没有裁定 daemon 重启并激活新配置后，既有 Run 使用旧快照、当前 daemon 快照还是首次建桶时的值。 | 配置变化会使同一 attempt 的 duplicate/accepted、桶容量、quota bucket 和 retry 结果分叉；`rate_limit_buckets` 已持久化旧参数时也没有合法的改写/拒绝规则。 | 明确这些参数全部从哪个不可变 snapshot 读取；按现有 Run 数据模型，宜统一读取 `runs.config_snapshot_id`。规定既有 bucket 的 capacity/refill 必须与该快照一致且不得被 daemon 当前配置重置，并覆盖“旧 Run + 重启后新配置 + 新 Run”的断言矩阵。 |
| R3 | **Report 子配额没有可插入 `budget_entries` 的完整身份。** report §6.2 只给 `kind/scope/scope_id/amount`；active storage §9.3 还强制要求 `bucket_start_ms`、`reason`、全局唯一 `operation_key`，而幂等与重放正依赖该 key。文稿也没有冻结日桶 end、DST/时区边界和收费 key 与 receipt/Interrupt 的绑定。 | 实现无法构造合法行；若各自发明 key，并发同 report 或事务重放可能重复收费或把两个直接 Interrupt 合成一笔。 | 冻结 Report charge 的完整 row shape：从冻结 timezone 得到的半开日桶、`reason` 枚举、由新 receipt/Interrupt 身份导出的 canonical `operation_key`，以及与 attention charge 的一一关系；同步 storage §9/§11，并加入 DST 边界、同 key/语义 duplicate、崩溃重放与并发收费 vectors。 |
| R4 | **`failure_review_once` 留下了待实现再同步的 generation 域，与现有 Interrupt 契约直接冲突。** report §6.2 要求按 `(run_id,daily_bucket_start_ms)` 每日一次，并明说未来再同步 interrupt；但 interrupt §5 规定 `failure_review` 唯一 preimage 必含 `(run_id,attempt_no,generation,failure_digest)`。同时异常所需的 `failure_class/failure_evidence_ref/recommended_action`、合法最低链接、attempt 是否为空、稳定 digest 与 golden key 均未给出。 | 跨 attempt 的同一 Run/日桶会生成不同 Interrupt，或调用方只能绕过唯一 `EmitInterrupt`；即使生成 key，现稿也无法构造通过 renderer/link 校验的确定性 `failure_review`。 | 在 report + interrupt + storage 一次冻结 Report quota-exhausted 的 domain/version/preimage、完整 typed fields 与 golden digest；给出 exact fallback facts、受控 evidence ref/links、attempt 绑定和事件/安全审计 identity。数据库唯一约束必须直接保证 `(run,day bucket)` 并发至多一条，而不是靠先查后写。 |
| R5 | **`RecordReport` 的可见结果矩阵自相矛盾。** §3 允许日志链接不合法时保留普通 blocker event；§5.1 又称 Interrupt 被合批/拒发时 receipt/event 仍保留；§6.2 要求 report charge、attention、receipt/event、Interrupt 全部成功或回滚；§7 则称任一步失败都无部分状态。子配额触顶分支是否消费 rate token、异常 `EmitInterrupt` 失败时安全事件是否与领域事务同提交也未定。active storage §11 对 `RecordReport` 只写“必要时原子发唯一异常 Interrupt”，没有承接普通 `agent_blocked` 的全部分支。 | 崩溃或配额边界上，实现可分别合法地产生“有 report 无 Interrupt”“全部回滚”“token 已扣但 report 未收”三种状态；这会改变限流、key 是否占用、时间线和人的打扰次数。 | 给出按顺序的 closed outcome 表：普通四类、blocker 不可形成 facts、blocker 可发且额度足、Report 子配额满、attention 合批/critical 熔断、`EmitInterrupt` 结构拒绝、事务内部错误。逐行冻结 rate token、report charge、receipt/key、domain event、安全事件、Run transition、Interrupt/attention/outbox 的提交或回滚，并同步 storage `RecordReport`/事务配方及崩溃注入断言。 |

## 3. 已通过的字段边界

- **能力与状态边界：通过。** 只连 `run.sock`、run token 不能调用 claim/ops、`completed` 不推进 Run，均与 ADR-006/008 和 active control-plane 一致。
- **Payload v1：通过。** 四类对象 closed，unknown/type/empty/NUL/`Cc`/换行拒绝，大小约束与 canonical digest 输入足以生成 schema 和 digest fixture。
- **两层去重：通过（事务前提待 R5 闭合）。** 同 key 异 digest 冲突，语义窗口新 key 返回原 receipt，duplicate 零新 key/receipt/event/收费，边界明确。
- **attempt 阶段：通过（客户端退避待 R1 闭合）。** 仅 `running` 接受、合法 `spawning` 暂时 `not_ready`，其他阶段和旧/跨代 binding 永久拒绝，与 DESIGN/control-plane 一致。
- **令牌桶算法：通过（参数来源待 R2 闭合）。** attempt scope、burst capacity、整数余数和重启连续性均引用 active storage 的单一算法，没有退回固定分钟窗口。

## 4. 非阻断注记

1. 成功、duplicate 与各错误的 CLI exit code、stdout/stderr 格式尚未冻结；实现前宜补最小机器可断言契约，尤其保证 payload/控制文件不被错误输出回显。
2. report event 的 `project_id`、`actor`、`idempotency_key` 以及 `occurred_at_ms/recorded_at_ms/received_at_ms` 等值关系未完全写明。建议以绑定 Run/project、服务端注入的同一接收时间和 receipt 作为唯一审计锚点补齐，避免时间线投影漂移。
3. `max_payload_bytes=1048576` 与 1 MiB RPC body 上限独立，故可传 payload 的实际上界会因 envelope 开销更小；这不是安全绕过，但 CLI 错误应区分 payload 超限与 frame 超限。

## 5. 验收判断

- 字段级评审完成：**YES**
- 遗留 P1：**5**
- `report.md` 转 `active`：**NO**
- 允许按现稿开始 Report 纵向实现：**NO**
- 可先实现：**与 R1–R5 无关的 payload closed schema/canonical digest、基础 run-token 授权测试 harness**
