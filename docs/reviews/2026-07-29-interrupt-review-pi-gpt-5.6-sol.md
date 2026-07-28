FAIL

# interrupt.md 字段级评审

> 日期：2026-07-29
> 评审人：pi × GPT-5.6-sol
> 评审对象：[`docs/specs/interrupt.md`](../specs/interrupt.md) draft
> 依据：PRD §4.1–§4.4/§5.5/§7.1、DESIGN §8.7/§10.1、WBS M3 §3.6–§3.7、[ADR-010](../decisions/010-attempt-spawn-handoff.md)、[ADR-013](../decisions/013-startup-stall-retry-convergence.md)
> 交叉核对：active [`storage.md`](../specs/storage.md) §6/§12.2/§12.5、[`config.md`](../specs/config.md) §3.9、[`outbox.md`](../specs/outbox.md) §2/§5

## 1. 结论

**FAIL。** 草稿已经覆盖七种 reason、`startup_stall` 隔离/事实仲裁、五件事事务和配置冻结等主要结构，但下列字段级缺口会让两个合规实现产生不同对象，或直接无法创建与 active outbox 契约兼容的 M3 发布 operation。`docs/specs/interrupt.md` 暂不能转为 `active`。

本结论只评审规格，不表示 M3 Runtime、Interrupt 发射器或 Command 已实现。

## 2. 阻断修改项

| 项 | 级别 | 发现 | 可关闭的修改项 |
|----|------|------|----------------|
| I1 | P1 | §3 的 fallback 表只冻结 option `id` 与简写 effect，没有冻结每项的 `label`、完整 `effect`、`risk`。`brief` 也只列“必含事实”，未规定字段顺序、缺省表达、Markdown 转义；links 未规定排序/去重。因而 §7 所要求的“相同输入产生同一字段值”不可判定，七种 fallback 也还不是完整 `Option {id,label,effect,risk}` 对象。 | 为七种 reason 冻结完整 option 四字段和 brief/links 的 canonical renderer：事实字段顺序、缺省规则、转义、链接排序与去重；补逐 reason golden vectors，证明无 T4/T6 时字段逐字节确定。 |
| I2 | P1 | §5 的 SHA-256 tuple 不含 domain/version 与 `reason`。`agent_blocked`、`failure_review`、`startup_stall` 等 tuple 具有相同字段数和兼容值形态，不同 reason 可散列同一 NUL tuple，被 `generation_key UNIQUE` 错误合并。另 §5 把 `startup_stall.cause` 定为 `process_identity_unknown | termination_unconfirmed | process_group_unverified` 一类失败分类，而 DESIGN §10.1 明定 `(run_id, attempt_no, generation, cause=startup_stall)`；四发现者究竟按一条还是按失败分类拆多条没有唯一答案。 | 将 generation preimage 冻结为带版本/domain 和 reason 的 typed tuple，并规定各字段禁止/编码 NUL；在 interrupt spec、DESIGN §10.1 与测试断言之间统一 `startup_stall` cause 语义。若保留细分 cause，须明确同一 attempt/generation 的不同失败分类是否允许多个 open Interrupt；若要求全程一条，则 cause 固定为 `startup_stall`，诊断分类另设事实字段。 |
| I3 | P1 | §5 声称“首次发布 operation”使用 `interrupt:<interrupt_id>:publish:0`，同时称 M3 以 forge comment 发布。active outbox §2 把该 key 专用于 `channel_publish`；`forge_comment` 必须使用 `comment:<purpose>:<subject_id>:<generation>`，且 payload/远端 marker 走 §5 协议。按草稿无法确定 M3 应创建哪个 operation kind/key，也无法满足既有 key→kind 契约。 | 明确 M3 首发为 `forge_comment`，冻结其 `purpose/subject_id/generation`、目标选择、payload 与 active outbox 一致的 operation key；把 `interrupt:<id>:publish:<escalation_no>` 保留给 M5 `channel_publish`，或同步版本化修改 outbox 契约，不能一键两 kind。同步修正 §1/§5/§7 的“发布 operation”断言。 |
| I4 | P1 | severity 契约内部矛盾。§1.2 规定 LLM/T4 不得改变 severity；§4.2 又允许 LLM“向下一级”，PRD §5.5 与 DESIGN §8.7 也允许降级。与此同时 §4.2 把唯一纯函数签名固定为四个确定性输入，签名中没有降级建议，无法表达后一句。 | 冻结一个唯一算法和接口：明确 M3 是否只算 base severity、M5 如何加入经过 schema 校验的“至多降级”输入，以及它发生在纯函数内还是确定性后处理；统一 §1.2、§2、§4.2 与验收用例，保证调用方不能直接指定最终 severity。 |
| I5 | P1 | escalation 提升存在边界歧义。§4.2 一面说“每个未封顶的 escalation 提升一级”，一面说 `escalation_count >= max_escalations` 时不再提升；当 count 恰好到上限时，最后一次升级是否提升没有定义。例如 max=2、count=2 可被实现为提升一次或两次。 | 用公式或逐值表冻结算法，例如基于 `min(escalation_count,max_escalations)` 计算提升步数，并覆盖 max=0、首次升级、恰达上限、超过上限和 critical 饱和测试。 |
| I6 | P1 | links 的必填语义互相冲突。§3 表头称“至少包含”Issue/Change 等链接，紧接着又允许缺项时省略；storage 允许 `links_json=[]`。仓库同时允许没有 Issue 的 manual Run，故 `design_approval`/`agent_blocked`/`failure_review` 不能无条件满足表中的 Issue 要求。当前也无法判断何时应拒绝发射。 | 逐 reason 区分“最低事实”与“可选链接”，冻结 manual Run 的替代链接/本地路径规则及允许空数组的条件；使 §1.4、§3、storage `links_json` 和负例验收使用同一判定。 |

## 3. 已对齐项

- reason 全集与 `min_modality` 对齐 PRD §4.3；`code_review=visual`、`startup_stall=text` 均保留红线。
- `startup_stall` 不提供 approve，只有 retry/reject/hold；retry 仍是 M5 的非终局探测请求，reject 不伪造执行体消失。
- `startup_stall` 禁止 `auto_reject`，升级封顶后 hold；与 PRD §4.2、ADR-013 和 config §3.9 一致。
- Run `waiting_human`、attempt 冻结、worktree 隔离与迟到事实优先窗口均对齐 DESIGN §10.1/ADR-010；终态不自动解除隔离。
- `attempt_resolution=reject | retry_after_absence`、`ResolveAttemptRace` 四调用点及 retry 成功 CAS 事务与 ADR-013/storage §12.5 一致。
- 首次发射五件事同事务、generation key 命中不重复收费，结构上对齐 DESIGN §8.7/storage §12.2。
- M3/M5 切片边界陈述正确：M3 只交付确定性 fallback 与 forge 可见面，不预支 T4/T6、Channel、tick/升级和 Command 执行。

## 4. 关闭检查清单

- [x] 字段级评审 `docs/specs/interrupt.md`
- [x] 对照 PRD 指定章节
- [x] 对照 DESIGN 指定章节
- [x] 对照 WBS M3 §3.6–§3.7
- [x] 对照 ADR-010/013
- [x] 输出规定路径的评审报告，首行结论为 `FAIL`
- [ ] `interrupt.md` 转 `status: active`（**NO：存在 I1–I6 阻断项**）
- [x] 未修改实现代码
- [x] 未自行创建实现 Issue

## 5. 复审入口

I1–I6 全部核销后，至少用以下 vectors 复审：七 reason 的完整 fallback golden object；跨 reason 同形 tuple 不碰撞；四发现者的 `startup_stall` 并发去重；M3 forge comment operation kind/key；severity 在 max=0/边界/封顶时的逐值结果；manual Run 无 Issue 时的 links 与拒发边界。
