---
status: active
created: 2026-07-28
summary: 外部副作用、幂等证据与重试收敛契约
---

# Outbox 规格

本文冻结 transactional outbox 的 operation payload、稳定键、claim/lease、逐类远端证据、错误分类与终态收敛。

结构来源：[DESIGN §6.3–§6.5](../DESIGN.md)、[ADR-003](../decisions/003-transactional-outbox.md)、[ADR-010](../decisions/010-attempt-spawn-handoff.md)、[ADR-011](../decisions/011-merge-requires-expected-head-cas.md)。表与事务见 [`storage.md` §8/§11–§13](storage.md)，launch handoff 见 [`control-plane.md` §4–§7](control-plane.md)，默认值见 [`config.md` §3.7–§3.8](config.md)。

## 评审处置

评审原文：[2026-07-28-outbox-review-pi-gpt-5.6-sol.md](../reviews/2026-07-28-outbox-review-pi-gpt-5.6-sol.md)。

| 发现 | 处置 |
|------|------|
| O1 过期 executing 无法 reclaim | 单 CAS 接管并补旧 attempt lease_expired result |
| O2 launch key 缺 generation | key 纳入 generation，旧 operation stale |
| O3 complete 不能承接领域效果 | outcomeCommand 原子写投影/event/Interrupt/后继 operation |
| O4 marker digest 自引用 | payload 不存 marker，worker 确定生成 |
| O5 Forge charge key 缺失 | 固定 attempt id + call sequence |

O1–O5 已关闭，评审通过，本规格转为 `active`。

Brain 字段级评审 [2026-07-28-brain-review-pi-gpt-5.6-sol.md](../reviews/2026-07-28-brain-review-pi-gpt-5.6-sol.md) 的交叉补丁：B2 为 `forge_comment` 新增 intake 澄清/确认目的与稳定 key（§2/§5.1）；B4 为 `forge_alert` 新增 token 越界告警目的（§5.1）。

## 1. 不变量

1. 外部 IO 前必须已有同领域事务提交的 outbox operation；worker 不临时补建 operation。
2. operation payload 创建后不可改；重试只更新 lease、attempt、证据、错误和调度字段。
3. 一个 operation 同时最多一个有效 lease；过期 worker 的结果整笔拒绝。
4. worker 每次真实 Forge API/CLI 调用前经 `ChargeForgeAPICall` 预留一次预算，包括证据查询；charge key 固定为 `forge-call:<outbox_attempt_id>:<call_seq>`，call_seq 从 1 递增。事务内不调用 Forge。
5. outbox 只保证至少一次尝试。effectively-once 必须由本规格逐 kind 的证据取得，不能由 operation key 推导。
6. 未知 payload version/kind、证据歧义、远端契约缺失均 fail closed，不降级为“再试一次看看”。
7. daemon 启动恢复完成前不得 claim `launch_agent`；其他 kind 可在数据库/配置恢复后执行。

## 2. Operation envelope

`outbox_operations.kind` 与 payload tagged union 一一对应。共同 envelope：

```json
{
  "schema_version": 1,
  "kind": "forge_comment",
  "project_id": "...",
  "created_from_event_id": "...",
  "body": {}
}
```

`schema_version` V0 为 1；`kind` 必须等于表列；`project_id` 可仅在 `channel_publish` 为 null。`created_from_event_id` 必须引用产生 operation 的事件。payload canonical JSON 的 SHA-256 必须等于表中 `payload_digest`。

稳定 operation key 只使用已冻结 ID/hash，不含时间、attempt_count、随机 request id 或可漂移文本：

| kind | operation key |
|------|---------------|
| `forge_comment` | `comment:<purpose>:<subject_id>:<generation>` |
| `forge_comment`（intake） | `comment:intake-clarification:<intake_id>:<generation>` 或 `comment:intake-duplicate-confirmation:<intake_id>:<generation>`；subject_id 为 intake_id |
| `forge_labels` | `labels:<subject_kind>:<subject_id>:<projection_version>` |
| `create_change` | `run:<run_id>:create-change:<head_sha>` |
| `merge_change` | `run:<run_id>:merge:<expected_head_sha>` |
| `rerun_checks` | `run:<run_id>:checks-rerun:<head_sha>:<check_run_id>:<retry_no>` |
| `channel_publish`（单 Interrupt） | `interrupt:<interrupt_id>:publish:<escalation_no>` |
| `channel_publish`（attention batch） | `attention-batch:<batch_id>:publish:1` |
| `launch_agent` | `run:<run_id>:attempt:<attempt_no>:generation:<generation>:launch` |
| `command_ack` | `command:<command_event_key>:ack`，其中 key 是 [`command.md` §1](command.md) 的 64 位 canonical event key |
| `forge_alert` | `alert:<alert_kind>:<subject_id>:<generation>` |

同 key 不同 payload digest 是 `contract_violation`，不得返回既有 operation 冒充成功。

## 3. Marker

Forge comment 与 Change body 使用同一不可见 marker：

```text
<!-- sift-op:v1:<base64url(UTF-8 operation_key)>:<payload_digest> -->
```

base64url 无 padding。渲染前必须从所有自然语言输入中移除形如 `<!-- sift-op:` 的片段；marker 只能由确定性 renderer 在末尾追加一次。查询时要求 operation key 与 digest 同时匹配：

- 同 key + 同 digest：本 operation 证据；
- 同 key + 异 digest：`semantic_conflict`；
- 多个对象命中同 marker：`semantic_conflict`，不任选其一。

Channel 无可靠查询面，使用可见标识 `[sift <operation_key>]`；重复投递对人可辨认但不宣称去重。

## 4. Worker 状态机

```text
pending/retryable --claim CAS--------------------> executing
executing + expired lease --reclaim CAS----------> executing (new owner/attempt)
executing --success------------------------------> succeeded
executing --transient/rate_limited--> retryable
executing --permanent contract--> failed
executing --fact changed--> stale
executing --ambiguous ownership--> conflict
```

通用 claim 事务必须同时：校验 pending/retryable 到期或 executing lease 过期 → reclaim 时为旧 attempt 插入 `retry/transient:lease_expired` result，并按该 kind 的投影协议（Channel 为 delivery/episode/alert）在同一事务更新。若该 result 后 `max_attempts>0` 已达限，operation 转 `failed`、清除 lease，且**不得**写新 lease、递增 `attempt_count` 或插入新 attempt；否则才写新 lease owner/expiry、`attempt_count+1` 和新 immutable `outbox_attempts`。不得先把过期 executing 异步改成 retryable；reclaim 是一次 CAS，旧 owner 随即失去 complete 权。`rerun_checks` 不适用此通用 reclaim，必须按 [`storage.md` §8.5](storage.md) 的 durable request-start 分支收敛。外部执行后 `CompleteOutboxAttempt(expectedLease, outcomeCommand)` 同时插入 immutable result、CAS operation，并按 kind 更新必要投影/事件：Create/Merge 更新 Run/Change 事实，Channel 更新 delivery，auth/capability 隔离项目并建唯一告警，conflict/stale 可原子产生 Interrupt/重算事件/后继 operation。不得先终结 operation、再另事务补领域效果。

lease owner 为 `<daemon_boot_id>:<worker_id>`。执行完成提交前必须仍匹配 owner 且 lease 未被新 owner替换；否则返回 `RejectedStaleWorker`，不插 result。lease 到期不证明外部动作未发生，新 owner 必须先走该 kind 的证据协议。

### 4.1 退避

瞬时失败第 n 次后的本地 delay：

```text
min(retry_max_delay, retry_initial_delay * retry_multiplier^(n-1))
```

只使用整数毫秒并向上取整，不加随机 jitter。`rate_limited` 若带可信 Retry-After，取 `max(local_delay, retry_after)`；它可超过 retry_max_delay，因为后者只约束本地退避。无论 completion 或 reclaim 的 immutable result 使 `max_attempts>0` 达到上限，operation 都转 `failed`；reclaim 不创建超限 attempt 或 lease。0 持续重试。semantic conflict、contract violation、stale 不消耗后续重试。

## 5. Forge comment / command ack / alert

### 5.1 Payload

三类共用 body：

```json
{
  "forge_kind":"github",
  "forge_host":"github.com",
  "forge_project_key":"owner/repo",
  "target_kind":"issue",
  "target_id":"123",
  "purpose":"interrupt",
  "markdown":"..."
}
```

- `forge_comment.purpose = interrupt | summary | intake_clarification | intake_duplicate_confirmation`；
- `command_ack.purpose = command_ack`；其 operation key 必须使用 `command_event_key`，不得使用项目内或跨 source 可能碰撞的裸 remote Forge ID。
- `forge_alert.purpose = channel_failure | project_isolated | config_drift | token_budget_exceeded | forge_api_budget_warning`；`channel_failure` 使用与 `forge_comment` 相同的必填 `markdown` 字段；其 canonical renderer 只读取持久化 operation key、episode generation/count、safe error class、delivery status 与已验证 Forge target，并追加固定的 `Diagnostics: sift ps; sift doctor` 行，禁止从当前 Run/Interrupt/config 补字段。
- payload 不存 marker，避免 digest 自引用；worker 在执行时由 operation key + 已冻结 payload digest 重算并追加 marker。

intake 目的的 comment payload 额外携带 `intake_id` 与 `generation`，outbox 行 `run_id` 保持 NULL，不伪造 run 关联；marker 与查询协议不变。该契约在 M1 冻结；真实 Forge comment worker、Intake 写端口接线及 crash marker 验收归属 [WBS M2 §2.3/§2.5](../WBS.md)，不能以 operation key/schema 已存在代替实现证据。

`token_budget_exceeded` 告警：按通用 key 格式 `alert:<alert_kind>:<subject_id>:<generation>` 取 `alert:token_budget_exceeded:global:<bucket_start_ms>:1`（subject_id 为 `global:<bucket_start_ms>`，generation 固定 1），每 UTC 日桶最多一个 operation；它仍走正常 attention 预算扣费，不得因 token 告警突破注意力配额。语义见 [`brain.md` §6](brain.md)。

`forge_api_budget_warning` 告警：项目每小时 forge API 消耗达到 `warning_ratio` 时由 `ChargeForgeAPICall` 同事务发出（[`forge.md` §9](forge.md)、[`storage.md` §9.1](storage.md)）。key 取 `alert:forge_api_budget_warning:project:<project_id>:<bucket_start_ms>:1`，subject_id 为 `project:<project_id>:<bucket_start_ms>`，generation 固定 1，每项目每 UTC 固定小时桶最多一个 operation（payload digest 稳定，仅含 purpose/project_id/bucket_start_ms，重复触发由 operation key 去重）。语义见 [`forge.md` §9](forge.md)。

### 5.2 执行

本执行协议随 M2 Forge 适配层落地；其中 Intake 澄清/确认评论的“远端成功、本地提交前崩溃”窗口是 M2 门禁。

1. 按 marker 查询目标评论；唯一命中则 succeeded 并保存 remote comment id。
2. 无命中才创建评论。
3. 创建成功后保存 id；本地提交前崩溃时重试回到第 1 步。
4. 查询不支持完整分页或返回被截断集合是 contract violation，不能据“没看到”创建。

评论被人删除后重试可能无证据；operation 已 succeeded 时不因后续删除自动重发。只有新的领域事件可创建新 operation。

## 6. Forge labels

Payload：

```json
{
  "forge_kind":"github","forge_host":"github.com","forge_project_key":"owner/repo",
  "target_kind":"issue","target_id":"123",
  "add":["sift:running"],"remove":["sift:queued"],
  "expected_projection_version":3
}
```

add/remove 各自排序去重且不相交。执行前本地目标 projection version 不同则 stale，不把旧状态标签写回。版本仍匹配时读取当前 label set，计算 `(current ∪ add) − remove`，使用平台 set/add-remove 幂等语义写入，再重读确认目标标签子集。与本 operation 无关的标签必须保留。

远端对象已不存在时根据已摄入外部事实收敛 stale；权限/能力缺失为 `auth_or_capability` 并隔离项目，不无限瞬时重试。

## 7. Create Change

Payload：

```json
{
  "run_id":"...","forge_kind":"github","forge_host":"github.com","forge_project_key":"owner/repo",
  "base_ref":"main","head_ref":"sift/run-...","head_sha":"...",
  "title":"...","body_markdown":"..."
}
```

执行协议：

1. 跨 open/closed/merged 全状态按 marker 搜索。
2. 唯一命中：保存 Change id/url/state/head；不创建第二个。
3. 无 marker 命中时，查询同 base/head 的全部 Change；存在 marker 不同或无 marker 的对象即 conflict，绝不接管。
4. 无冲突才创建；body 必须含本 operation marker。
5. 创建响应的 marker/id/head 不一致为 contract violation。
6. 远端已 closed/merged 的 marker 命中按外部事实推进 Run，不重新创建。

成功证据：

```json
{"schema_version":1,"change_id":"...","change_url":"...","state":"open","head_sha":"...","marker":"..."}
```

首次保存 Change id 后，后续对账只按 id；按 marker 搜索仅用于“远端成功、本地 id 未提交”的窗口。head 变化形成新 operation key；旧 operation 收敛 stale，不得以同 key 改 payload。

## 8. Rerun Checks

Payload：

```json
{
  "run_id":"...","change_id":"...","head_sha":"...",
  "check_run_id":"...","retry_no":1,"triage_source_digest":"..."
}
```

只接受 Gate `retry_checks/flaky_retry` verdict 创建的 payload。`retry_no` 从 1 开始，且 operation key、payload 和 Gate snapshot 的 head/check/retry number 必须完全一致；创建 operation、记录已消费 retry number 和 Gate evaluation 在同一事务提交。worker 调用 [`forge.md` §4.12](forge.md) 的 `RerunCheck` 前，每一次读/调都照常收费。

此 kind 没有 marker 或可查询的 exactly-once 证据。worker 在每次实际 `RerunCheck` 前必须调用 [`storage.md` §8.5](storage.md) 的 `MarkOutboxAttemptRequestStarted`；仅在该 lease-CAS 事务成功提交后才能发请求。request-start 不存在时的调用前 transient/rate-limit 才可转 retryable；一旦该不可变事实存在，收到成功、错误、进程崩溃或 lease 过期都不得再次调用。成功后 operation succeeded 并触发重新观测 Checks；`SemanticConflict`/`AuthOrCapability`/调用结果不明均为 conflict，原子创建 `failure_review` Interrupt。reclaim 对已 request-start 的旧 attempt 直接 conflict，不创建新 attempt。这一保守规则保证单一 Gate retry number 最多导致一次远端 rerun，宁可人工处理也不突破额度。

## 9. Merge Change

Payload：

```json
{
  "run_id":"...","change_id":"...","gate_evaluation_id":"...",
  "expected_head_sha":"...","merge_method":"merge"
}
```

`expected_head_sha` 必须等于引用 Gate snapshot 的 head，operation key 也含该 SHA。执行：

1. 预读 Change：已以 expected head 合并则 succeeded；当前 head 不同则 stale，并触发新 head 重新冻结 Gate；closed 未合并按外部事实收敛。
2. 当前 head 相同时发**远端条件 merge**，请求自身携 expected head；预读不可替代条件请求。
3. 远端 CAS mismatch → stale；不重试旧 operation。
4. 适配器无 expected-head CAS capability → `auth_or_capability`/project isolated；禁止无条件 merge。
5. 成功后按 Change id 重读 merged state/head/merge sha 并持久证据。

`merge_method` V0 只能为 `merge`；未来新增方法须版本化并验证平台语义。

## 10. Channel publish

`channel_publish` 的 `body.delivery_kind` 是 closed `interrupt | attention_batch` tagged union。Channel failure 的 successor `forge_alert` 也必须使用 §5.1 的 closed body（包括必填 `markdown`），不得使用仅 target/purpose 的旧形态；exact canonical payload/digest 见 [`storage.md` §6.6](storage.md#66-channel-batch-and-failure-episode-exact-vectors)。`channel.target_ref` 唯一为非秘密 `secret_ref:<name>` resolver handle，绝不是 URL/endpoint；adapter 每次 attempt 用 sealed handle 解析当前 secret，而 payload、digest、日志和诊断不保存解析值。单 Interrupt payload 为（Channel 选择和渲染上下文来自 Interrupt 冻结 snapshot，不从当前 config 重建）：

```json
{
  "delivery_kind":"interrupt","delivery_id":"interrupt:<interrupt_id>:<escalation_no>:<channel_id>","interrupt_id":"...","escalation_no":0,"priority":"normal",
  "interrupt_version":1,"nonce":"...",
  "channel":{"id":"...","type":"webhook","target_ref":"secret_ref:<name>","capabilities":["text"],"renderer":"plain-v1"},
  "rendered_text":"..."
}
```

attention batch payload 只能由 [`storage.md` §6.3、§6.6`](storage.md) 的 `PrepareAttentionBatch` 从 sealed member 快照生成；§6.6 的完整 Forge target、跨项目拒绝与响应丢失 replay payload 是本节唯一 exact fixture；其 `channel` snapshot 和 `forge_alert_target` 必须与 batch 冻结 identity 一致，不能由 Channel worker 拼接或改写：

```json
{
  "delivery_kind":"attention_batch","batch_id":"daily:<project_id>:<zone>:<due_at_ms>:<channel_id>:<forge_kind>:<base64url(forge_host)>:<base64url(forge_project_key)>:<target_kind>:<base64url(target_id)>","delivery_id":"<batch_id>:publish:1","batch_kind":"daily_summary",
  "channel":{"id":"ops-slack","type":"webhook","target_ref":"secret_ref:<name>","capabilities":["text"],"renderer":"plain-v1"},
  "project_id":"...","forge_alert_target":{"forge_kind":"github","forge_host":"github.com","forge_project_key":"...","target_kind":"issue","target_id":"..."},
  "scope":"day","scope_id":"Asia/Shanghai:1785286800000","due_at_ms":1785286800000,
  "members":[{
    "delivery_id":"<batch_id>:<interrupt_id>","interrupt_id":"...","interrupt_version":1,"nonce":"...","headline":"...",
    "reason":"agent_blocked","severity":"high","links":[],"options":[],"command_lines":[]
  }],"rendered_text":"..."
}
```

`members` 按 `interrupt_id` UTF-8 bytes 排序，至少一项；每项只引用原 Interrupt 的 frozen headline/links/options/nonce/version，且 `command_lines` 是该 nonce 下逐 option 的完整 Command。摘要没有可执行的 batch action，也没有 summary reason：人必须以成员的 `interrupt_id + nonce + option` 回复，任何试图对 `batch_id` 执行 Command 都拒绝。payload 的 batch ID、delivery ID、channel snapshot、kind/scope、due_at 和 members 必须与 sealed batch 完全一致；member delivery ID 必须逐字节等于持久化 member；不匹配为 `contract_violation`。它携带由 operation key 生成的可见标识，响应丢失重放相同的 immutable payload。

worker 由 operation key 生成并追加可见标识；payload 不接受调用方 marker。Channel 无查询证据，每个 executing attempt 都可能真实推送；语义明确为 at-least-once。成功响应只证明本次调用返回成功，不证明未重复。

每个 immutable Channel operation 有一条 durable `channel_failure_episodes` 行：单条 `subject_id=interrupt_deliveries.delivery_id`，batch `subject_id=batch_deliveries.delivery_id`，两者 `generation=1`。`ClaimOutboxOperation` 的 reclaim CAS 与 `CompleteOutboxAttempt` 是唯一写端口；reclaim 将 immutable `lease_expired` result 和 delivery/episode/alert 投影同事务提交；仅未达 `max_attempts` 时同一 CAS 才创建新 lease/attempt，达限则终结 operation/delivery/episode 且不创建它们。completion 则将普通 result 与同样投影同事务提交：`transient`、`rate_limited` 和 reclaim 的 `lease_expired` 均增加连续失败；其他 failed result 也增加一次并以 `ended_failed` 终结，success 清零并以 `ended_delivered` 终结。由 0 达到 config `channel_failure_alert_after` 时，事务以 `alert:channel_failure:<subject_id>:1` 创建一个 `forge_alert`，并更新 delivery/doctor 投影。只有 retry policy 仍允许时继续原 operation；`max_attempts` terminal 后保持“已生成、未送达”及 alert 可见。lease CAS 串行化并发 completion，重启读 episode 行恢复；alert 自身失败不递归创建 alert。batch alert 使用 sealed batch 的单一 verified Forge target，batch 不跨项目或 target，定义和 vectors 见 [`storage.md` §6.3、§6.6`](storage.md)。

escalation 使用新 operation key但复用原 attention charge；同 escalation 重试不新扣费。batch member 的 `quota_batched` admission 没有虚构 charge；其 delivery 审计使用 admission ID，指标去重使用 storage 的 stable `metric_identity`，而不是可能新增的 critical admission ID。

## 11. Launch Agent

Payload不含任何 capability 明文：

```json
{
  "run_id":"...","attempt_no":1,"generation":1,"backend":"process",
  "run_dir":"...","worktree_path":"...","task_spec_snapshot_id":"...",
  "agent":{"id":"...","executable":"...","args":[],"task_transport":"stdin"}
}
```

协议：

1. 恢复门禁通过后 claim operation lease。
2. 生成 dispatch id、run token、bootstrap nonce；调用 `PrepareLaunchDispatch` CAS 保存 dispatch 与 hash。
3. 按 [`control-plane.md` §7.1](control-plane.md) 原子写 bootstrap。
4. backend 只 spawn wrapper，bootstrap path 是唯一新增 argv；不把 credential 放 argv/env。
5. operation 的“外部执行成功”不是 wrapper spawn 返回，而是 `claim.acquire` 已绑定 session；handler 必须调用 `AcquireLaunchClaim`，把 pending→starting、session hash、immutable outbox result 与 operation succeeded 原子提交。后续 permit/started 由 control-plane/storage 推进。

崩溃收敛：

- prepare 前：普通 lease 重试；
- prepare 后、bootstrap 前：无 owner/control，旧 operation → stale；同事务递增 generation并创建含新 generation 的 launch operation，绝不修改旧 payload；
- bootstrap 后、spawn 前：新 owner校验并复用文件/dispatch；
- spawn 后、acquire 前：竞争 wrapper只有一个可绑定 session；
- acquire 后：operation succeeded，attempt 恢复接管 starting/spawning，绝不重放 launch。

`spawning` 后 effectively-once 的强度来自 session/permit/进程组消失证据，不来自 outbox lease。

## 12. 结果与错误分类

| class/outcome | operation state | 领域动作 |
|---------------|-----------------|----------|
| success | succeeded | 保存证据并推进必要投影 |
| transient | retryable | 退避 |
| rate_limited | retryable | honor Retry-After |
| auth_or_capability | failed | 项目隔离 + 唯一告警 |
| contract_violation | failed | 安全事件；必要时 failure_review |
| semantic_conflict | conflict | 转唯一 HITL，不接管远端对象 |
| stale | stale | 吸收新事实；必要时重算 Gate/transition |

错误 summary 必须去除 CLI stderr 中的 token、URL query credential 与控制文件内容；原始 stderr 不入事件或 outbox。每个 attempt 均恰有一个 immutable result；worker 崩溃留下的 attempt 由 reclaim 写 `lease_expired` result。

## 13. M1 骨架与后续 kind

M1 必须完整实现通用 claim/complete、退避、immutable attempts/results 与 `launch_agent`；其他 kind 的 payload decoder、operation key builder 和 fake adapter 契约在 M1 建立，随对应里程碑启用。不得用一个 `map[string]any` payload 占位后绕过 schema。

生产接线由 `cmd/siftd` 的唯一 `startSchedulers` 负责：提交后的 `DB.SetOutboxWakeup` 唤醒独立 Outbox scheduler，启动和 supervisor 时钟只补偿重启后的 durable retry deadline，不能代替提交唤醒。`startSchedulers` 返回前等待 outbox startup sweep 完成，令后续 commit wake 与 startup wake 可判别；`cmd/siftd/main_test.go` 通过 production wiring 和真实 `Daemon.OutboxTick` 覆盖 `EnqueueOperation` 与 `EmitInterrupt` 两条写口，并验证移除 wake hook 后不能由迟到 startup wake 推进。`TestOutboxCommitWakeupClaimsWithoutPeriodicTick` 仍是 storage seam 测试，不单独声称 production wiring。kind/project 过滤后的 claim 先按每个 Run 最近一次 append-only outbox attempt 序位轮转（无 Run 的 operation 自成 identity），再按 due time/id 排序；`outbox_operations_run_fairness` 支撑该查询，避免 hot Run 在持续 backlog 下饿死其他 Run。

## 14. 验收

1. operation 与领域投影同事务；各写点崩溃只能全有或全无。
2. payload 创建后数据库 trigger 拒绝修改；同 key 异 digest 为 contract violation。
3. 过期 executing 可被单 CAS reclaim；claim/lease/complete 拒绝旧 owner，每次已结束尝试有且仅有一个 immutable result。`rerun_checks` 的 request-start 前崩溃可 reclaim；request-start 后 lease 过期必须 conflict 且不得创建新 attempt或第二次调用。
4. comment/ack/alert 在“远端成功、本地提交前崩溃”后按 marker 收敛，不重复创建。
5. create Change 全状态 marker 搜索；同 base/head 非本 operation 对象必须 conflict。
6. merge 的预读相等但远端 CAS mismatch 仍 stale；无 CAS capability 不自动 merge。
7. labels 不删除非 Sift 标签；重复执行得到同集合。
8. 单 Interrupt 或 immutable attention batch 的 Channel 注入响应丢失可产生可辨认重复；达到失败阈值只创建一个 forge alert且不递归。并发入批只产生一个 stable batch operation，sealing 前关闭成员不在 payload 中，batch Command 不能批量执行。
9. launch 在 prepare/bootstrap/spawn/acquire 每个边界崩溃均不签发两个 permit、不双起有效 Agent。
10. 每次 Forge 证据查询和动作调用均唯一收费；预算不足时不发起调用。
11. max_attempts、指数退避与 Retry-After 使用注入时间可确定测试。
12. fake adapter 覆盖 success/transient/rate-limit/auth/contract/conflict/stale 全分类。
13. `EnqueueOperation` 与 `EmitInterrupt` 提交后由 production 注册的 outbox wakeup 推进真实 kind worker，不等待 supervisor tick；三具名 scheduler 的定向测试验证 wakeup 不串联，重启后的 retry deadline 仍由持久化 `next_attempt_at_ms` 收敛。

## 15. 评审冻结项

1. comment marker 必须在 GitHub/GitLab 评论与 Change body 中无损保存并全分页搜索。
2. create Change 必须同时执行全状态 marker 查询和同 base/head 冲突查询。
3. 平台不能提供 expected-head merge CAS 时必须禁用 auto merge。
4. launch operation 在 acquire 原子绑定 session 时 succeeded；started 属 attempt handoff，不延长 outbox lease。

## 16. 自查结果

- [x] 九类 operation 均有稳定 key、closed payload 与明确投递语义；`rerun_checks` 明确为最多一次远端调用；attention batch 以真实 batch/member 生成 immutable payload，intake 评论与 token 告警目的不伪造 run 关联、不突破注意力配额。
- [x] effectively-once 声明均有 marker/set/CAS/handoff 证据；Channel 如实为 at-least-once。
- [x] create Change 不接管同 base/head 的人工对象；merge 不以预读替代远端 CAS。
- [x] launch payload 无 capability 明文，prepare/file/spawn/acquire 窗口有唯一恢复动作。
- [x] Forge 查询与动作均经过唯一收费口。
- [x] 相对链接存在、代码围栏闭合、无尾随空白。

**自查结论：** 字段级契约完整，评审通过，允许转 `active`。
