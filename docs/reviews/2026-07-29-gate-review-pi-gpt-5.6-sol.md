# gate.md 字段级评审

> 日期：2026-07-29
> 评审人：pi × GPT-5.6-sol
> 评审对象：[`docs/specs/gate.md`](../specs/gate.md) draft
> 依据：PRD §4.1/§4.3/§5.4/§5.6、DESIGN §6.4–§6.5/§8.5、ADR-004/ADR-011、WBS M4 §4.3，以及 active 的 forge/storage/outbox/interrupt 规格

## 1. 结论

**FAIL（6×P1）。** 候选稿正确冻结了纯函数、整快照摘要、判定顺序、一次性/remembered 豁免边界、Shadow Gate 事务和 Change marker/CAS 原则，但仍是行为级提纲，不是可生成 closed schema、可逐字节回放的字段级契约。`gate.md` 必须保持 `draft`，本报告不授权 M4 实现方自行选择缺失字段或枚举。

本次直接修正了不需要产品裁决的三处问题：补齐事实域中此前漏列的 state/路径完整性/pending 证据/重试计数/T5 来源/风险阈值等必需输入；修正 `auto_merge=true` 时 conflicting/unknown 不得冒充“全绿但不自动合并”；把一次性豁免从循环依赖的“绑定整份 Gate 输入”收窄为稳定的 Run/head/rule/命中路径 digest。其余阻断涉及跨规格身份、外部副作用或产品枚举，不能用评审者猜测闭合。

## 2. P1 阻断

| 项 | 发现 | 为什么阻断 | 关闭条件 |
|----|------|------------|----------|
| G1 | **没有 Gate input v1 的 closed schema。** §2.1 只有“至少包含”的事实域，未冻结对象层级、required/null、枚举、长度/数值范围、排序去重、交叉约束；也未定义路径集合不完整、pending 起点未知、T5 仅在 failure 出现等表示。 | 不同实现会生成不同 canonical JSON/hash；空数组可被误读为“无变更路径”，直接绕过 hard guardrail；回放无法验证同一输入。 | 在本文逐字段冻结 `GateInputV1`，给出 closed JSON Schema 的单一生成源与 invalid fixtures；至少覆盖 Change/Checks/T5/effective policy/risk/identity/一次性豁免。 |
| G2 | **`verdict` 不是 exact tagged union。** “至少表达”没有 kind/code/payload schema，也未固定 hard failure、wait、retry、四类 HITL、merge、全绿不合并的互斥关系；未知 Checks、pending timeout、draft、closed/merged Change 的结果不唯一。 | `verdict_digest`、cache 冲突检查、outbox payload 和 Interrupt 必需事实均无法生成；调用方可把同一事实解释为不同动作。 | 冻结 `VerdictV1` 的全部 kind、每分支 required 字段、禁止字段、公共 head 绑定与 canonical fixture；明确每个输入矩阵只命中一个分支。 |
| G3 | **protected path 契约未到字段级。** 默认清单仍引用“等价 CI 配置”，但没有穷尽或发现规则；自定义 rule 没有 `rule_id/pattern/level` schema；glob 方言、路径规范化、大小写、hard/soft 重叠优先级、多个 soft hit 的稳定选择均未定义。 | 平台/库的 glob 差异会造成 hard guardrail 漏放；无法构造 interrupt 的 `violation_code` 和稳定 generation key。 | 冻结 V0 默认 hard rule ID/模式全集、受限 matcher 和 normalization；若支持自定义 CI 路径，定义其可信来源及取不到时的 fail-closed 语义；定义 hit 排序与 hard-wins。 |
| G4 | **“flaky 且有额度则确定性重试”没有可提交的副作用契约。** active Forge 只有 13 个动词，无 rerun Checks/Pipeline 动词；active outbox 的 8 类 operation 也无 checks retry。远端 rerun 通常没有 marker/CAS，响应丢失后盲重试还可能突破 `flaky_retry_limit`。 | Gate 即使返回 retry verdict，M4 也没有符合 transactional-outbox 纪律的执行口；直接在 reconciler 调 Forge 会违反既有 IO/崩溃约束。 | 明确 V0 重试究竟重试什么；若为远端 CI rerun，先在 forge/outbox/storage 冻结动词、payload/key、head 绑定、额度收费和“调用成功但本地未提交”语义，再由 verdict 引用该 operation；不得用普通可重试 operation 猜 effectively-once。 |
| G5 | **Gate 消费的规范化 effective policy 仍未冻结。** `policy.md` 明示字段全集留给 #216；现稿未定义 risky 阈值字段、protected rule/remembered exception 形态、timeout 单位和范围。config 只有四个 defaults，甚至没有 risky threshold。 | `review_policy=risky-only` 没有比较阈值，函数不可实现；policy hash 与 Gate input hash 无共同字节基准。 | #216 冻结 base/effective policy v1 后，本规格明确引用同一个生成类型并列出 Gate 实际消费字段；风险阈值必须有唯一默认、范围和 `>=`/`>` 边界。 |
| G6 | **Gate evaluation、calibration 与 Interrupt 的身份接缝矛盾。** storage 要求每次 evaluation 都插入 `predicted_decision NOT NULL`，Ledger 只接受 `allow|block`，但 wait/retry/非终止结果如何映射未定义；`interrupt.md` 的 guardrail generation 要 `policy_snapshot_id`，当前存储和 policy 规格只有 `effective_policy_hash`/Gate snapshot，没有该对象。 | `RecordGateEvaluation` 无法对所有 verdict 原子写入合法 calibration；soft guardrail HITL 也无法从已定义事实生成 active Interrupt key。 | 与 #217 一起冻结每个 verdict 到 `allow|block|不可比较` 的映射，并使 storage nullable/枚举与“每次调用都有 calibration”一致；统一 guardrail generation 身份为真实存在且不可变的 ID/hash，更新规范向量。 |

## 3. 已采纳的非阻断修正

| 项 | 级别 | 处置 |
|----|------|------|
| G7 | P2 | 输入事实域显式补 state、路径完整性、pending 时间证据、flaky 已用次数、T5 分类/来源、policy 风险阈值及完整 T3 输出，防止后续 closed schema继续漏字段。 |
| G8 | P2 | §3.4 区分“策略关闭/draft 的全绿不合并”与 `auto_merge=true` 下 conflicting/unknown；后两者不得记录为全绿。 |
| G9 | P2 | 一次性豁免改绑 `(run_id, head_sha, rule_id, canonical matched-path digest)`；禁止绑定包含豁免自身的整份 `gate_input_hash`，消除批准后必然 cache miss 且永远不匹配的循环。 |
| G10 | P3 | §7 明示行为验收不替代 closed schema，避免 checklist 被误读为字段审定。 |

## 4. 字段级核对

- **纯函数与缓存：通过（原则层）。** 无 IO/时钟，`(gate_input_hash, gate_version)` 唯一缓存键，cache hit 仍新增 evaluation/calibration，方向正确。
- **输入 schema：不通过。** 缺 closed object、字段约束、排序和交叉矩阵；G1/G5 阻断。
- **输出 schema：不通过。** 缺 exact union 和 digest fixture；G2 阻断。
- **护栏：不通过。** hard 不可豁免原则正确，但 matcher/default/rule/hit identity 未冻结；G3 阻断。
- **Checks/T3/T5：不通过。** fail-closed 原则正确，T3/T5 来源进入快照正确；flaky retry 无副作用端口；G4 阻断。
- **HITL/Shadow Gate：不通过。** 同事务方向正确，但二元预判和 generation identity 无法落表；G6 阻断。
- **Change 创建/合并：通过（本规格范围）。** 只消费 M3 成功事实、全状态 marker、远端 ID 收敛和 expected-head CAS 与 active forge/outbox 一致。

## 5. 交叉评审边界

- #216 必须提供 Gate 可消费的 effective policy v1；本报告不抢先替 policy 评审选择用户可写字段。
- #217 必须对齐 calibration 的二元/不可比较映射和每次 evaluation 的记录要求。
- #218 需确认 T5 输出字段，但 **G4 不是 Brain schema 能解决的问题**：它缺的是远端 CI 重试副作用及崩溃语义。
- active `interrupt.md` 的 guardrail generation identity 需在 G6 关闭时同步修正并重算规范向量。

## 6. 验收判断

- `gate.md` 转 `active`：**NO**
- P1 数量：**6**
- 评审报告入库：**YES**
- 允许开始 Gate 实现：**NO；可先实现与 schema 无关的纯函数/fixture harness，但不得冻结私有字段作为事实标准**
