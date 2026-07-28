PASS WITH NOTES

# interrupt.md 三次字段级评审

> 日期：2026-07-29
> 评审人：pi × GPT-5.6-sol
> 评审对象：[`docs/specs/interrupt.md`](../specs/interrupt.md)（#101/#102 修订后）
> 二次评审：[`2026-07-29-interrupt-rereview-pi-gpt-5.6-sol.md`](2026-07-29-interrupt-rereview-pi-gpt-5.6-sol.md)
> 依据：PRD §4.1–§4.4/§5.5/§7.1、DESIGN §8.7/§10.1、WBS M3 §3.6–§3.7、[ADR-010](../decisions/010-attempt-spawn-handoff.md)、[ADR-013](../decisions/013-startup-stall-retry-convergence.md)
> 交叉核对：active [`storage.md`](../specs/storage.md) §5–§6/§12.2/§12.5、[`config.md`](../specs/config.md) §3.9、[`outbox.md`](../specs/outbox.md) §2/§5

## 1. 结论

**PASS WITH NOTES。** 二次评审的 R1–R4 均已核销，未发现阻止 M3 按字段实现的剩余歧义。`docs/specs/interrupt.md` 可转为 `status: active`。

本结论只确认规格已达到实现基线，不表示 M3 Runtime、Interrupt 发射器、超时升级或 Command 已实现。

## 2. R1–R4 对账

| 项 | 结论 | 核验结果 |
|----|------|----------|
| R1：换行 Escape | **YES** | §3.2 对 CRLF、裸 CR、裸 LF 分别规定拒绝码，明确不得规范化或保留换行；三条拒绝 vector 均不生成 Interrupt、预算或 operation。其余 Unicode `Cc` 也被拒绝。 |
| R2：golden vectors / severity | **YES** | §3.5 已明确限定为 renderer 子对象，只覆盖 `reason/headline/brief/min_modality/links/options`，不声明对象级 severity；§7 同步使用“renderer 子对象”，与 §4.2 的四种 `high` base 不再冲突。 |
| R3：generation preimage | **YES** | §5 冻结实际 NUL 分隔的 typed UTF-8 协议、完整前缀、字段顺序和值编码；`enum:cause` 与值 `startup_stall` 分离；`git_oid` 支持 40/64 位小写 object ID，`sha256` 则唯一为 64 位小写 hex。独立重算七条完整 preimage 的 SHA-256，均与文档 digest 一致。 |
| R4：manual discussion target | **YES** | `storage.md` §5.2 已增加同空同非空且插入后不可变的 `discussion_target_kind/id/url`，创建前按绑定 project 经 Forge 验证并同 Run 冻结；interrupt §3.4 和 storage §12.2 均要求 outbox payload 只从已验证 Issue、Change 或冻结 target 产生。storage §16 #8 与 interrupt §7 覆盖 manual、无 Issue、尚无 Change时成功生成 `forge_comment`，并覆盖缺失/非法/修改拒绝。 |

## 3. 交叉一致性

- M3 首发仍为 `forge_comment`，key 为 `comment:interrupt:<interrupt_id>:1`；`interrupt:<id>:publish:<escalation_no>` 只属于 M5 `channel_publish`，与 outbox §2/§5 一致。
- `startup_stall` 的生成身份仍固定为 `(run_id, attempt_no, generation, cause=startup_stall)`；诊断分类不拆条，且 `auto_reject` 被 interrupt/config 双重禁止。
- Run 转人工、attempt/worktree 隔离、五件事同事务、迟到事实仲裁及 retry 成功 CAS 边界与 storage/ADR-010/ADR-013 一致。
- 七种 reason 的 options、最低媒介、最低链接与可发布目标是独立约束；合法 `links=[]` 不会绕过 comment target 要求。

## 4. 非阻断注记

1. 二次评审附注的“完整 forge comment markdown 模板”仍未冻结：当前已冻结 Interrupt renderer 子对象及 outbox payload/marker，但 headline/options/links 组合成最终 markdown 的逐字节模板尚待实现前补齐或明确 renderer 版本边界。这不影响 R1–R4 的字段对象、generation key 或 operation target 唯一性。
2. §3.2 分别冻结了单类 CRLF/CR/LF 的拒绝码；若一个值同时含多类换行，发射必然拒绝，但具体诊断码优先级未明示。实现可在测试化前补一条优先级规则，避免诊断观测差异；该差异不会产生 Interrupt 或任何部分事务。

## 5. 关闭检查清单

- [x] 三次字段级评审 `docs/specs/interrupt.md`
- [x] 对照二次评审报告逐项核销 R1–R4
- [x] 对照 PRD/DESIGN/WBS、ADR-010/013 与 active storage/config/outbox 契约
- [x] 独立重算并核对七条 generation key vectors
- [x] 报告写入规定路径且首行为 `PASS WITH NOTES`
- [x] `interrupt.md` 转为 `status: active`
- [x] 未修改实现代码
- [x] 未自行创建实现 Issue
