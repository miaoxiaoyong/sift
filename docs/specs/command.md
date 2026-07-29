---
status: draft
created: 2026-07-29
summary: Forge 指令的鉴权、解析、确定性效果与回执契约
---

# Command 规格

本文定义 M5 Command：经鉴权的 Forge 评论和审批标签如何成为一个可重放的领域命令。本文保持 `draft`；它冻结字段和事务边界，不表示已实现或已通过评审。

来源：[PRD §7.1、§9.2](../PRD.md)、[DESIGN §6.2、§6.4、§10.1](../DESIGN.md)、[WBS M5 §5.4](../WBS.md)、[ADR-013](../decisions/013-startup-stall-retry-convergence.md)。Interrupt、配置、Forge、存储、Ledger 和 outbox 的权威字段分别见 [`interrupt.md`](interrupt.md)、[`config.md`](config.md)、[`forge.md`](forge.md)、[`storage.md`](storage.md)、[`ledger.md`](ledger.md) 和 [`outbox.md`](outbox.md)。

## 1. 不变量与身份

1. Command 只消费 `forge_comment` 的 `/sift` 候选和 `approval_label` 的新增事件；关闭、合并等 Forge 事实仍走 Intake。
2. 每个候选先以 canonical command event key 去重，再鉴权、解析、精确匹配 immutable target、当前 Interrupt、nonce/cutoff 与 `options[]`，最后由唯一存储事务端口提交。不得从当前 Run、Forge 标签集合、最近评论或自然语言猜测。
3. canonical key 是 `SHA-256(canonical_json({"v":1,"project_id":project_id,"source":source,"remote_event_id":remote_event_id}))` 的 64 位小写 hex。`source` 严格为 `forge_comment | approval_label`，remote ID 为 1–256 UTF-8 bytes 且不含 NUL。该 key 是 receipt、事件 `idempotency_key`、probe requester 关联和 ack operation 的**同一**身份；不得单独拼接远端 ID。
4. allowlist 只鉴权，不绕过 CAS、target、nonce、cutoff、Gate、隔离或 options。非命令、缺 actor 和不可信 actor 静默；公开文本不得含 actor、token、旧 nonce、原评论、reject/ask 原文、进程身份、消失证据或数据库错误。
5. Command 不取得 `*sql.Tx`，也不在事务中调用 Forge、Brain、进程检查或信号。唯一 public command 写端口为 `ApplyCommandEvent(envelope, parsed_action?)`；其私有事务原语负责 Ledger、状态和 outbox。`startup_stall` probe 的最终结果仅由 `ApplyRetryProbeResult` 提交。

## 2. 输入 envelope、Forge 边界与 receipt

### 2.1 `CommandEventEnvelopeV1`

可信与不可信的**候选**均先被 Forge 保留为下列 closed envelope；未知字段、重复 JSON key 或不满足大小限制均拒绝。`actor` 是显式 nullable，不能由其他字段补全。

```json
{
  "schema_version": 1,
  "event_key": "64-lowercase-hex",
  "project_id": "…",
  "source": "forge_comment",
  "remote_event_id": "…",
  "target": {"kind":"issue","id":"123"},
  "actor": "alice",
  "raw_digest": "64-lowercase-hex",
  "comment": {"id":"…","body":"/sift approve …"},
  "label": null,
  "label_position": null
}
```

Required common fields are as shown. `target.kind` is `issue|change`; `target.id` is 1–256 bytes. `comment` is required only for `forge_comment`, where `comment.id=remote_event_id`, `body` is 1–16384 UTF-8 bytes, and label fields are null. For `approval_label`, `label={"event_id":remote_event_id,"name":"…","action":"added"}` and `label_position` are required, while `comment` is null. `label_position` is a canonical positive decimal integer (no sign/leading zero, at most 39 digits). `raw_digest` hashes the unmodified source payload. `event_key` is recomputed, never trusted from the adapter.

[`forge.md` §2/§4](forge.md) requires both platforms to provide stable comment/note ID, label-event ID, exact target, nullable actor and label position. A driver event lacking target, remote ID or source is a Forge contract violation, not a Command input. Missing actor remains an envelope with `actor=null`; Command owns its persisted ignored receipt.

### 2.2 Receipt and candidate boundary

`forge_event_receipts` uses `(project_id,event_kind,forge_event_id)` where `event_kind` is exactly the envelope `source`; it stores `event_key`, target, nullable actor and raw digest. A duplicate returns the stored outcome and creates no event, Ledger entry, probe or ack.

| candidate | immutable receipt | domain event / ack |
|---|---|---|
| not `/sift` and not configured approval-label addition | no receipt | none |
| actor null | `ignored_missing_actor` + low-sensitivity security event | none / no ack |
| actor not in startup config snapshot allowlist | `ignored_untrusted_actor` + low-sensitivity security event | none / no ack |
| trusted candidate | `accepted`, linked to its initial command event | §6 mapping; exactly one ack except pending retry |

A syntax failure is still a trusted candidate: it atomically writes `accepted` receipt, a closed rejection event and its ack. All receipt/event/ack writes named in this document occur in one transaction or not at all.

### 2.3 Immutable command target

Each Interrupt has exactly one `interrupt_command_targets` binding to the initial `forge_comment` publish operation. The binding contains its immutable `(kind,id)` and a real unique FK to that operation; it is created in `EmitInterrupt`'s five-things transaction from the same verified target used in the payload. Command compares envelope target only with this binding. It must never reconstruct a target from the current Run, Issue, Change, delivery, comment text or Forge query.

## 3. Authentication, grammar and approval labels

The allowlist and `labels.approved` come only from the Run's immutable startup `config_snapshot_id`. `labels.approved` is normalized by the config loader as an exact nonempty UTF-8 label (no trim, case fold or platform rewrite); its scope is the Run project and its Forge platform. Config file changes take effect only on the next daemon boot and only for newly created Runs/Interrupts; they cannot change a pending command's label.

### 3.1 Byte grammar

All literals below use one ASCII space (`SP`, byte `0x20`); `EOF` immediately follows the final byte. `run_id` and `nonce` are exactly 32 lowercase hex bytes.

```abnf
command  = approve / reject / retry / hold / ask
approve  = "/sift approve" SP run-id SP nonce EOF
reject   = "/sift reject" SP run-id SP nonce [SP reason] EOF
retry    = "/sift retry" SP run-id SP nonce EOF
hold     = "/sift hold" SP run-id SP nonce SP duration EOF
ask      = "/sift ask" SP run-id SP nonce SP text EOF
run-id   = 32lowerhex
nonce    = 32lowerhex
lowerhex = DIGIT / %x61-66
reason   = 1*16384utf8-no-nul
text     = 1*16384utf8-no-nul
; reason contains no CR or LF; text may contain LF or CRLF exactly as supplied.
duration = 1*(duration-number duration-unit)
duration-number = 1*DIGIT ["." 1*DIGIT] / "." 1*DIGIT
duration-unit = "ns" / "us" / %xC2.B5 "s" / "ms" / "s" / "m" / "h"
```

`utf8-no-nul` is valid UTF-8 with no NUL; the 16384 limit is bytes after the one separating SP. `reason` additionally rejects CR and LF. `ask` alone can be multiline. The duration grammar is the positive, unsigned subset of Go `time.ParseDuration`: parse it with that function, reject an overflow or a result `<=0`, then compare it to the Interrupt's immutable `hold_max_duration_ms`. No other Go-duration spelling is accepted.

`hold_max_duration_ms` is copied from `attention.hold_max_duration` into the Interrupt when it is created, alongside expiry defaults. Thus restart/config drift cannot change a pending hold limit. Parser vectors must cover one/many/missing SP, EOF, optional reject reason, CR/LF cases, each duration unit/decimal, overflow, zero/negative/sign, and exact lower/upper limit.

### 3.2 Label anti-replay

Only `label.name == labels.approved` and `action == added` may request `approve`. Label commands never manufacture a nonce. On nonce issuance/rotation, the storage port first makes label approval unavailable (`approval_label_cutoff_position=NULL`), then Forge fully scans that target's approval-label stream outside a transaction and `SetApprovalLabelCutoff(interrupt_id,version,nonce,position)` CAS-freezes its high-water position. Until this second transaction succeeds, or on a platform without the required position capability, label approval fails closed.

A label is current exactly when its `label_position > approval_label_cutoff_position`; equality and all earlier positions reject. The position is a platform-proven, target-scoped remote monotonic label-event sequence, not a daemon or Forge wall-clock. Cursor replay, same-position input, crash between nonce rotation and cutover, and a full scan that cannot prove a high-water position all remain unavailable/rejected. A person may remove and re-add the configured label after label approval becomes available. This intentionally may reject a label added in the conservative scan window; it must never accept a pre-issuance label.

## 4. Closed compilation and deterministic effects

A parsed command is optional input to the single transaction, not the transaction's complete input:

```text
CompiledCommandV1 {
  envelope: CommandEventEnvelopeV1,
  action?: approve|reject|retry|hold|ask,
  run_id?, nonce?, hold_duration_ms?, reject_reason?, ask_text?,
  interrupt_id?, expected_run_version?, expected_interrupt_version?
}
```

The executable fields are populated only after the immutable target, one open Interrupt, current nonce/cutoff and option validation. The transaction loads its own current snapshots and inserts the command event; callers cannot provide target status, options, calibration ID, Task Spec ID, severity, outbox key or SQL.

For every successful row below, `recordHumanDecisionTx` writes exactly one `HumanDecisionV1`; reject/ask additionally write the unmodified `SemanticMaterialV1`. `hold` always retains `waiting_human`, updates expiry to `occurred_at + duration`, increments Interrupt version and rotates nonce. All unlisted reason/action pairs reject as `rejected_option`.

| reason | action | immutable binding/precondition | one deterministic transaction effect |
|---|---|---|---|
| `design_approval` | approve | pre-start Run; no live attempt | close Interrupt/responded; `waiting_human→queued`; create next pending attempt/claim and one launch operation |
|  | reject | — | close/responded; `Run→failed(human_reject)` |
|  | hold | — | common hold |
| `guardrail_violation` | approve | binding has exact run/head/rule/matched-path digest | consume that one-time exemption; close/responded; retain Run waiting only long enough to enqueue exactly one Gate re-evaluation for that binding |
|  | reject | — | close/responded; `Run→failed(human_reject)` |
|  | hold | — | common hold |
| `code_review` | approve | binding has exact change ID/head/review-policy snapshot | insert one immutable human-review approval for that binding; close/responded; enqueue exactly one Gate re-evaluation of that head |
|  | reject | — | close/responded; `Run→failed(human_reject)` |
|  | hold | — | common hold |
| `agent_blocked` | ask | current attempt binding | insert Task Spec snapshot sourced by command event; close/responded; terminalize bound blocked attempt; create next pending attempt/claim and launch |
|  | retry | current attempt binding | close/responded; terminalize bound blocked attempt; create next pending attempt/claim and launch without Task Spec change |
|  | reject | — | close/responded; `Run→failed(human_reject)` |
|  | hold | — | common hold |
| `merge_conflict` | retry | exact change/head binding | close/responded; enqueue one Gate re-evaluation of that exact head; no attempt or merge operation is created |
|  | reject | — | close/responded; `Run→failed(human_reject)` |
|  | hold | — | common hold |
| `failure_review` | retry | binding declares exactly `gate_recheck` or `new_attempt` | close/responded; respectively enqueue one exact-head Gate re-evaluation, or terminalize bound failed attempt and create next pending attempt/claim/launch |
|  | reject | — | close/responded; `Run→failed(human_reject)` |
|  | hold | — | common hold |
| `startup_stall` | retry | §5 request CAS | §5 only |
|  | reject | §5 race CAS | §5 only |
|  | hold | §5 race CAS | common hold through the private race primitive |

`interrupt_command_effect_bindings` is immutable, one-to-one with an Interrupt, and has a closed reason-tagged schema for the bindings in this table (including `failure_review.retry_kind`). It is written by the reason owner in the same transaction that emits the Interrupt; unknown/missing/cross-reason binding rejects. Gate re-evaluation is a persisted deduplicated internal operation keyed by `(interrupt_id, expected head where applicable)`, not a Forge call inside this transaction. Approval/review/exemption facts are consumed by the next Gate snapshot; they never overwrite the historical snapshot.

## 5. `startup_stall`: one owner, request then result

`startup_stall` options are only `retry|reject|hold`; label approval cannot reach it. `ApplyCommandEvent` owns its request and reject paths and calls private `resolveAttemptRaceTx`; no separately callable Command/Ledger race port exists.

- **retry request:** atomically CAS Run version, attempt generation, open Interrupt version/nonce and no live probe; write initial `command.accepted` event, receipt, retry HumanDecision, one pending probe and `dispatch_state=probe_in_progress`. It does not close the Interrupt, create an attempt, release isolation, write resolution or ack. The probe worker runs after commit.
- **probe failed:** one transaction marks probe failed, retains Interrupt/open isolation, increments version and rotates nonce (or applies frozen capped hold), appends final outcome and creates the one ack with `absence_unconfirmed`.
- **fact wins:** the same private race primitive marks probe superseded, closes the Interrupt `superseded_by_fact`, appends final outcome and creates its one `superseded_by_fact` ack.
- **probe succeeds:** `ApplyRetryProbeResult` alone runs the ADR-013 transaction: evidence, `retry_after_absence`, isolation release, close/responded, `waiting_human→queued`, exactly one next attempt/claim/launch, final outcome and one `applied` ack are all-or-nothing.
- **reject:** the same private race primitive atomically writes `attempt_resolution=reject`, closes/responded, fails Run, retains isolation, writes one HumanDecision/Ledger event/final outcome/ack, and schedules controlled termination. A late fact is absorbed as `superseded_by_decision`, never revives the Run or releases isolation.

While probe is in progress every later human candidate gets one final `probe_in_progress` rejection; no second probe is made. The request command has no ack before a final probe/race result, so it can never create both an acceptance and a completion acknowledgement.

## 6. Events, acknowledgements and replay

### 6.1 Closed schemas

`CommandEventV1` is a closed, canonical JSON object (maximum 64 KiB):

```json
{"schema_version":1,"event_key":"…","source":"forge_comment","remote_event_id":"…","outcome":"rejected_syntax","action":null,"run_id":null,"interrupt_id":null,"next_nonce":null,"final_for_event_id":null}
```

All fields shown are required. `event_key` and source match the envelope; `remote_event_id` is copied; `outcome` is one of the mapping table; `action`, `run_id`, `interrupt_id` are nullable with the same limits as §2; `next_nonce` is null or 32 lowercase hex; `final_for_event_id` is null except a final retry outcome, where it references the one `retry_pending` event with the same event key. It contains no natural-language input. Initial retry uses `outcome=retry_pending`; final retry uses this second closed event, not an ambiguous replacement.

`CommandAckV1` is also closed/canonical and at most 8 KiB:

```json
{"schema_version":1,"command_event_id":"…","action":null,"disposition":"rejected_syntax","run_id":null,"interrupt_id":null,"next_nonce":null}
```

All shown fields are required. `command_event_id` references the final `CommandEventV1`; disposition is one of the non-pending outcomes below. Non-null action/run/Interrupt must equal that final event. Unknown fields, noncanonical JSON or incompatible nulls reject.

| branch | final event type / outcome | ack disposition |
|---|---|---|
| grammar, malformed label shape | `command.rejected` / `rejected_syntax` | `rejected_syntax` |
| no unique/current target, unavailable/old/equal label cutoff | `command.rejected` / `rejected_target` | `rejected_target` |
| nonce/version/CAS stale | `command.rejected` / `rejected_stale` | `rejected_stale` |
| action not in options | `command.rejected` / `rejected_option` | `rejected_option` |
| probe already live | `command.rejected` / `probe_in_progress` | `probe_in_progress` |
| normal table action, startup reject, successful probe | `command.accepted` or `command.resolved` / `applied` | `applied` |
| retry request | `command.accepted` / `retry_pending` | no ack yet |
| probe cannot prove absence | `command.resolved` / `absence_unconfirmed` | `absence_unconfirmed` |
| execution fact wins | `command.resolved` / `superseded_by_fact` | `superseded_by_fact` |
| decision already won | `command.resolved` / `superseded_by_decision` | `superseded_by_decision` |

The final ack operation key is `command:<event_key>:ack`; its `created_from_event_id` is the final event and its target is the immutable envelope target. Same key with another digest is a contract violation. It is created in the same final transaction, not after an external probe.

The deterministic renderer outputs action, disposition, Run and Interrupt. When `next_nonce` is non-null it may output **only the newly issued current nonce** in a complete executable command for that same target; it must never echo the submitted/old nonce. That new nonce is a public anti-replay correlator, not a capability secret.

## 7. Required fixtures and crash vectors

M5 tests must cover GitHub/GitLab comment and label IDs, exact targets, nullable actors, full label pagination/high-water positions and cross-project/cross-source equal remote IDs. They must also cover every grammar and matrix row, default/non-default approved label, config restart drift, label cutover/replay/crash, all table mappings, and nonce renderer bytes.

For every trusted candidate, crash injection at receipt, initial/final command event, Ledger, Run/Interrupt, Task Spec/exemption/review fact, probe, launch and ack operation proves all-or-nothing. Replaying after each point returns the persisted final outcome and at most one Ledger decision, probe, state effect and ack operation.
