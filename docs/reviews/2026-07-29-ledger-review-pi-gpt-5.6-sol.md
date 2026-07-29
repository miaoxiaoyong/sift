# ledger.md 字段级评审

> 日期：2026-07-29
> 评审人：pi × GPT-5.6-sol
> 评审对象：[`docs/specs/ledger.md`](../specs/ledger.md) draft
> 依据：PRD §4.1/§4.5/§5.6/§5.9/§10.2–§10.3、DESIGN §8.2/§8.5–§8.6、ADR-004、WBS M4 §4.4，以及 active 的 config/storage/interrupt 规格和同片 draft gate 规格

## 1. 结论

**FAIL（6×P1）。** 候选稿正确守住了 append-only、A7 聚合边界、响应间隔禁作注意力成本、手工合并事实优先、校准/认证同事务等原则；但记录对象仍以“至少含”的文本列表表达，且预判、动作关联和认证版本与相邻规格存在不可实现的矛盾。`ledger.md` 必须保持 `draft`，不得据此冻结私有 JSON 形态或开始认证实现。

本次已直接修正三类不需要产品裁决的问题：provenance 改为带 kind 的真实身份，避免把 Gate evaluation ID 冒充 event ID；人类动作补齐 `approve/reject/retry/hold/ask/manual_merge/manual_close` 并明确只有终局、因果关联的动作才映射校准结果；送达记录改为成功 delivery 的幂等事实，并冻结首次送达响应间隔和合批约束。

## 2. P1 阻断

| 项 | 发现 | 为什么阻断 | 关闭条件 |
|---|---|---|---|
| L1 | **四类 `features_json` 没有 closed schema。** envelope 与各 entry 均使用“至少含”或字段名列表，缺对象层级、required/null、枚举、长度/数值范围、数组排序去重、交叉约束和 unknown-field 拒绝规则；`change_size` 甚至未选分桶还是统计对象。 | storage 要求每个 JSON 列按 schema version 校验；当前无法生成 schema、canonical fixture 或迁移，也无法证明 calibration 与 Ledger 的特征字节一致。 | 在本文给出 `GateSampleV1/HumanDecisionV1/SemanticMaterialV1/AttentionDeliveryV1` 的 closed schema 单一生成源、有效/无效 fixture、大小上限及 canonical 规则链接。 |
| L2 | **Shadow prediction 与运行期 verdict 混为一谈，且“每次调用有 calibration”与不可比较结果冲突。** Gate 可能 wait/retry/HITL；ledger 说不可比较时不得伪造二元预判，storage 却要求 `predicted_decision NOT NULL`，gate/WBS 又要求每次调用新增 calibration。更深一层，若把 `code_review` 或因尚未认证而关闭的 auto-merge 直接映射为 `block`，系统会循环地产生误拦，永远无法取得开启 auto-merge 的证据。 | 无法为每次 Gate evaluation 插入合法行；即使强行映射，认证统计也会被当前策略资格污染，形成自证不能的闭环。 | Gate 输出须有独立于运行期 action/verdict 的 exact `shadow_decision: allow | block | inconclusive` 及语义；storage 允许并约束 `inconclusive`；ledger 明确只有二元样本可结算，并给全部 Gate verdict 分支的映射矩阵。 |
| L3 | **Gate evaluation、Interrupt 与人的动作没有不可变因果绑定。** `recordHumanDecision` 接受可空身份并只检查同 Run；同一 Run/head 可有多次 evaluation/calibration，Interrupt generation 去重后也可能返回既有 Interrupt。“取最新一条”或调用方任选都符合现文。 | 人回复可能补全并非其看到的预判，污染漏放/误拦；重放顺序不同还会选择不同样本。 | 在 storage 冻结真实关系（例如 Interrupt 创建时不可变绑定唯一 calibration/evaluation，非 Gate Interrupt 为空），并规定命令只能解析该绑定；不得按时间或“最新”猜测。 |
| L4 | **Gate sample 有两份权威副本且 Gate 事务未承诺写 Ledger。** §2.2 同时称 `gate_sample` 是权威副本，又允许 `calibration_entries.features_json` 保存“同一特征或引用”；storage 没有引用列，`RecordGateEvaluation`/§12.6 只列 snapshot/evaluation/calibration，未列 `ledger_entries`。 | 实现可合法选择复制、JSON 内伪引用或根本不写 gate_sample；两份不可变事实若不同也无仲裁规则。 | 选择唯一物理事实源：要么 calibration 以真实 FK 指向同事务创建的 gate_sample entry，要么删除 gate_sample 复制并由 snapshot+calibration 派生；同步 storage 写端口、FK/唯一约束和崩溃事务。 |
| L5 | **认证版本模型跨规格自相矛盾，旧 Gate cache 可能继续命中。** ledger/DESIGN/ADR 要求任一影响资格的样本使 `certification_version` 改变；config §3.12 定义该值只等于“算法版本 + canonical 配置”的 hash；policy §4沿用后者；storage 又以 `(task_kind, certification_version)` 为主键原地更新计数。 | 样本使 `certified` 从 false 变 true 时，若版本不变且其他 Gate 输入相同，`gate_input_hash` 可不变并命中资格变化前的旧 verdict；若每样本改 version，现有 config 定义与投影主键又错误。 | 拆分并命名“规则/配置版本”与“证据投影 revision/digest”，两者共同进入资格快照与 Gate input；规定 revision 的确定性生成和更新，统一 ledger/config/policy/storage。 |
| L6 | **认证窗口和比率公式不可执行。** 现文未定义 `false_block_rate` 分母、`total_samples_min` 的参与条件、窗口边界、以 predicted 还是 decided 时间归窗、零分母、迟到决定、task kind 枚举及历史窗口淘汰后的增量算法；“误拦上限由配额与吞吐共同约束”也未说明 config 的固定 `false_block_rate_max` 如何兑现。 | 两个实现可对相同 ledger 得出不同 `certified`，而该布尔会开启 auto-merge，属于安全关键分歧。 | 冻结 exact 公式（含 `leak/negative` 与 `false_block/total` 或经产品确认的其他分母）、全部 minimum、半开窗口和时间列、零分母及迟到样本；明确 V0 固定阈值与配额反推的配置/提案边界，并给边界 fixture。 |

## 3. 已采纳的非阻断修正

| 项 | 级别 | 处置 |
|---|---|---|
| L7 | P2 | provenance 改为 `{source:{kind,id}}`，限定 `event | gate_evaluation | interrupt_delivery`，不再把 evaluation ID 填入名为 `source_event_id` 的字段。 |
| L8 | P2 | 人类记录改为 action + 可空 calibration decision；`retry/hold/ask` 与 Gate 前 design approval 不再伪造 `allow/block`，`ask` 明确只写语义原料与 Task Spec。 |
| L9 | P2 | `attention_delivery` 只在实际成功送达后按 delivery ID 幂等追加；合批字段互斥，响应间隔固定取首次实际送达，负间隔为 null。 |
| L10 | P3 | 将已存在的 gate 规格改为显式链接，删除“后续 gate.md”的过期措辞。 |

## 4. 字段级核对

- **A7 与用途边界：通过（原则层）。** 类别级资格不读取单条结论，T7 只提案，响应间隔不作成本。
- **记录 schema：不通过。** L1/L4 阻断 schema 生成和单一事实源。
- **预判与人类结果：不通过。** 动作全集经本次修正，但 shadow decision 语义和因果绑定仍由 L2/L3 阻断。
- **认证统计：不通过。** 分类方向正确；版本、窗口和公式由 L5/L6 阻断。
- **手工合并与指标分母：通过（行为层）。** 有预判则保留校准并标 bypass；无预判不伪造；不进入 Sift 发起合并的误放行率分母。
- **注意力/语义原料：部分通过。** 送达与 ask/reject 原料边界已明确，完整 closed schema 仍受 L1 阻断。

## 5. 交叉评审边界

- gate review 的 G6 与 L2/L3 是同一接缝：必须由 gate + ledger + storage 一次闭合，不能仅把 storage 列改成 nullable。
- L5 需要同步 config §3.12、policy §4 与 storage §10.2/§10.6；只在 ledger 发明第二个版本名仍会留下缓存漏洞。
- Command 的具体语法可留 M5，但 M4 必须先冻结可持久化 action union、Gate 因果身份和校准映射；Command 不得自行选择。

## 6. 验收判断

- `ledger.md` 转 `active`：**NO**
- P1 数量：**6**
- 评审报告入库：**YES**
- 允许开始 Ledger/认证实现：**NO；可先实现与 schema 无关的 append-only/事务测试 harness**
