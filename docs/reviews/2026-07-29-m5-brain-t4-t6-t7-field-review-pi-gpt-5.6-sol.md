PASS WITH NOTES

# brain.md T4/T6/T7 字段级评审

> 日期：2026-07-29
> 评审人：pi × GPT-5.6-sol
> 评审对象：[`docs/specs/brain.md`](../specs/brain.md) §11–§13（PR #294 / Issue #300）
> 依据：[PRD §5.3、§5.5、§5.9](../PRD.md)、[DESIGN §8.3、§8.6–§8.7、§9.2](../DESIGN.md)、[WBS M5 §5.1](../WBS.md)
> 交叉核对：[`interrupt.md`](../specs/interrupt.md)、[`ledger.md`](../specs/ledger.md)、[`storage.md` §6/§10](../specs/storage.md)、[`config.md` §3.9](../specs/config.md)、[`outbox.md` §10](../specs/outbox.md)

## 1. 结论

**PASS WITH NOTES（0 个遗留 P1）。**

PR #294 的权限方向正确：T4 只改展示，T6 只建议调度，T7 只写待人审 draft；三者都复用统一 call/attempt、closed decode、token、版本与 fallback 壳，A7 也以类型和写口隔离而非 prompt 自律实现。但原稿仍有会让两个实现生成不同 schema、让 T4 改写 canonical action 顺序、让 T6 调用方任意选择兜底阈值，以及让 project evidence 生成 global proposal 的字段缺口。

本 worktree 已同步关闭全部 P1/P2：

- T4 option IDs 改为与 canonical input 同序全集，模型只能推荐 ID；
- T4 冻结纯文本 renderer，模型文本不能注入 Markdown/HTML/outbox marker 或命令；
- T6 增冻结时间，v1 fallback 阈值固定为 `high`，quota snapshot 与 modality-compatible Channel 集闭合；
- T7 使用版本化、可解析 aggregate identity，闭合 category/replay/semantic evidence 与计数约束；
- T7 `target_scope` 必须等于 aggregate scope，单项目证据不能提升为 global；
- 三触点 normal/fallback source union、terminal-call 一致性与 fallback reason 枚举冻结；
- T7 每个 valid call 至多写一条 immutable `pending_human_approval` draft，重放只能 insert-or-return identical。

`brain.md` 文件头原已因 T1/T2/T3/T5 为 `active`；本次移除 §11–§13 的局部 draft 标记并将 T4/T6/T7 契约升为 `active`。这不表示 M5 实现、prompt/schema asset、proposal storage migration 或 M5 门禁已经完成。

## 2. 发现与处置

| ID | 级别 | 发现 | 处置 |
|---|---|---|---|
| MBB1 | P1 | T4 允许模型任意重排 `options`，与 canonical Interrupt option 顺序和“LLM 不改写动作”边界冲突；相同领域对象会因模型排序产生不同可执行展示。 | §11.2 改为 IDs 必须与 input 逐项同序相等；推荐只经 `recommended_option_id` 表达，完整 `Option` 始终从确定性 input 组装。 |
| MBB2 | P1 | T4 的 conclusion/key points 可直接进入 Markdown brief，却没有精确 escaping；模型可生成 HTML、链接、`<!-- sift-op:` marker 或看似可执行的 `/sift` 文本。 | 冻结 `EscapeT4Text` 字符集与纯文本 sink 规则；brief 只由 escaped 文本和 canonical label 组装，headline 在 markup sink 同样转义。 |
| MBB3 | P1 | T6 用“本次冻结时间”校验 expiry/next window，但 input 没有该字段，回放无法重建；`fallback_immediate_min_severity` 又允许每次调用者从四档任取，所谓确定性 fallback 没有唯一值。 | §12.1 增 `frozen_at_ms` 及时间不等式；v1 threshold 固定为 `high`，改变必须版本化。fallback 明确为 high/critical immediate、low/normal batch。 |
| MBB4 | P1 | T6 Channel candidates 只要求“可用”，未要求满足 `min_modality`；attention remaining 也未规定项集，模型可收到 visual 不兼容 Channel、缺档/重复档或虚构 critical quota。 | candidates 限为已配置、可用且 modality-compatible；default 必须来自同一集合；remaining 固定为同序 `low,normal,high` 三项，critical 禁止出现。无兼容 Channel 时不调用 T6，既有 Forge 首发有效且 delivery held。 |
| MBB5 | P1 | T7 aggregate key 以冒号拼接未受限 project ID，scope/category/window 也未与 input/trace 交叉绑定；同一 evidence window 可拥有歧义 identity。 | §3 改为 `aggregate:v1:global:...` / `aggregate:v1:project:<project_id_b64url>:...`；窗口、category、project_id 与 trace 逐字段绑定，并同步收紧 `storage.md` §10.1。 |
| MBB6 | P1 | T7 output 可在 project aggregate 上建议 `target_scope=global`，把单项目样本提升为全局策略/context 提案；global aggregate 也无法唯一指出 project target。 | §13.2 固定 target scope 与 aggregate scope 相等；project 只能提该 project，global 只能提 global。 |
| MBB7 | P2 | category `evidence_summary` 类型未定义，categories 上限大于 Run kind 全集；replay counts 无分子/分母关系，跨来源 evidence ID 可碰撞，无法生成唯一 closed schema/citation validator。 | §13.1 冻结 1..5 个类别、summary 全字段、digest/version 格式、count inequalities、semantic item bounds 和全输入 evidence ID 唯一性。 |
| MBB8 | P2 | T4/T6/T7 source object 只是示例；normal/fallback 字段类型、terminal call 一致性及 reason enum 未冻结。 | 各节冻结 closed tagged union，要求引用对应 terminal call，fallback reason 统一为六值枚举且等于 call 记录。 |
| MBB9 | P2 | T7 只说“持久化 proposal draft”，没有同一 valid call 重放时的唯一性与变更边界；实现可能重复提案或原地改写审批状态。 | §13.2 冻结 closed persistence shape、`logical_call_id UNIQUE`、constant pending status、insert-or-return-identical；批准/拒绝另写审计记录，不更新 draft。 |

## 3. 字段级核对

### T4

- **Input：通过。** 顶层与 nested object 均 closed；Run/attempt、reason/severity/modality、fallback brief、已验证 links 和 canonical options 有类型、bounds、排序及逐字节领域绑定。
- **Output：通过。** headline/conclusion/key points/recommendation 均有长度和控制码约束；option IDs 只能同序回显，effect/risk 永不来自模型。
- **渲染与 fallback：通过。** 模型文本只能走固定纯文本 escaping；任一 decode/domain/provider/budget failure 整体回退原始状态与 links，不拼接半份模型输出，仍由唯一 `EmitInterrupt` 收费、去重和发布。

### T6

- **Input：通过。** `frozen_at_ms` 使 expiry/window 可回放；availability nullable 关系、完整 quota snapshot、兼容 Channel 集和 default identity 均闭合。
- **Output：通过。** delivery 三态、候选 Channel、至多一级降级及 critical/availability/expiry 交叉约束明确；没有“不发出”、任意时间戳、凭据或 quota 授权字段。
- **Fallback：通过。** v1 阈值唯一为 high；结果仍经过 availability、severity、generation、配额和 critical fuse，token 耗尽不能借支人的注意力。

### T7 / A7

- **Input：通过。** aggregate scope/category/window/project identity 可唯一解析；类别认证、replay 计数和语义原料均为 closed、排序、去重、全局唯一 evidence references，且不携当前 Run/Gate/Interrupt identity。
- **Output：通过。** 只有 proposal kind/scope/title/body/evidence/constant approval；没有 patch、path、verdict、action、Interrupt 或状态字段，project evidence 不能提权到 global。
- **写口：通过。** valid call 只能 insert-or-return 一条 immutable pending draft；fallback 不写 draft；Gate、Interrupt、certification、policy loader 与 Context writer 均不在该端口能力面。
- **A7：通过。** 同一冻结 Gate input 的 verdict 与单条 HITL 不得读取或受 T7 output、proposal、semantic material 或历史变化影响；只有独立人审后形成的新有效配置可影响后续冻结输入。

## 4. 非阻断注记

1. **N1（交叉 draft）：** `interrupt.md` 仍处于 #297 的独立字段评审范围，其 §7 使用 `dispatch=...defer`，而 Brain 权威 schema 使用 `delivery=...next_window`；T4 的简报接纳字段也需按本次 `conclusion/key_points/recommended_option_id` 接缝同步。Brain 字段权威现已唯一，故不阻断本 spec 激活，但 M5 接线前 #297 必须消除命名和生命周期漂移。
2. **N2（未实现）：** T4/T6/T7 prompt、生成 schema、proposal draft migration、展示/调度关联审计和集成测试均尚未交付。active 表示实现基准稳定，不是实现证据。
3. **N3（人审后继）：** 本次只冻结 T7 的“不生效 draft”写口。人如何批准/拒绝、如何形成独立 policy Change 或 context edit 仍须由 M5 的 Command/Forge 流程给出审计化入口；在该入口完成前，proposal 只能保持 pending，不能用临时自动落盘补洞。

## 5. 验收判断

- P1 遗留：**0**
- P2 遗留：**0**
- 结论：**PASS WITH NOTES**
- `brain.md` §11–§13 转 `active`：**YES**
- T1/T2/T3/T5 active 契约回退：**NO**
- M5 Brain/Attention/T7 实现完成：**NO**
- M5 阶段门禁通过：**NO**
