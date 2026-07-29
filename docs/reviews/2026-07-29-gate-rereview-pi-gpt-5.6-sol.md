# gate.md G1–G6 定向复审

> 日期：2026-07-29
> 评审人：pi × GPT-5.6-sol
> 评审对象：[`docs/specs/gate.md`](../specs/gate.md) draft
> 基线：[`2026-07-29-gate-review-pi-gpt-5.6-sol.md`](2026-07-29-gate-review-pi-gpt-5.6-sol.md) 的 G1–G6
> 闭合提交：PR #222（G1–G4）、#225（G5）、#231（G6），以及 PR #232 的 Ledger 复审

## 1. 结论

**FAIL（3×P1）。** G3、G5、G6 已关闭；G1、G2、G4 仍各有一处会让实现方自行选择语义的阻断。`docs/specs/gate.md` 必须保持 `draft`，本报告不授权开始 Gate 实现或转 `active`。

问题不是缺少更多说明性文字：当前输入契约同时要求“接受路径不完整的显式表示”和“拒绝建立该 snapshot”；判定矩阵没有处理 `closed|merged` Change；`rerun_checks` 又要求持久化 `request-started` 后按特殊规则回收，但 storage 没有该事实，通用 reclaim 反而会创建新 attempt。这三处都无法由测试从现有规格唯一推导。

## 2. G1–G6 对账

| 项 | 结论 | 证据与判断 |
|---|---|---|
| G1 Gate input closed schema | **未关闭（P1）** | [`gate.md` §2.2](../specs/gate.md#22-gateinputv1-闭合形态g1) 的字段表允许 `paths_complete=false` 且要求 `changed_paths=[]`，§3.1 也规定该输入返回 `input_unknown`；但 §2.2 末段又规定“无法完整取得路径”拒绝建立 snapshot。单一生成源无法同时把同一对象定义为合法 Gate 输入和 snapshot 前置错误。必须明确 `paths_complete=false` 是否属于 `GateInputV1`，并只保留一个 fail-closed 入口及对应 invalid/valid fixture。 |
| G2 exact verdict union | **未关闭（P1）** | input 冻结了 `change.state=open|closed|merged`，但 [`gate.md` §2.3、§3](../specs/gate.md#23-verdictv1-闭合并集g2) 没有 closed/merged 分支，也没有在判定顺序中先拒绝非 open Change。相同 closed/merged 输入可被实现为 `input_unknown`、终止、等待 Checks，甚至继续到 merge 阶段；原 G2 明列的 closed/merged 唯一结果仍未冻结。必须新增 exact branch 或把非 open 变成有唯一错误码的输入约束，并补分支/反例 fixture。 |
| G3 protected paths | **已关闭** | [`policy.md` §2.1、§3.2](../specs/policy.md) 冻结受限 glob、路径规范化及 V0 内建 hard 全集；[`gate.md` §3.1](../specs/gate.md#3-判定顺序) 冻结派生 rule ID、排序、hard-wins、soft exception 和多命中选择。没有残余平台 glob 或“等价 CI 配置”自由度。 |
| G4 flaky rerun side effect | **未关闭（P1）** | Gate/outbox 要求“一旦开始 `RerunCheck`”即永久标记、lease 过期直接 conflict，且 reclaim 依据旧 attempt 的 `request-started` 事实；但 [`storage.md` §8.3–§8.4](../specs/storage.md) 的 attempt/result 无 `request_started`（或等价不可变阶段事实），通用 lease-expiry 规则会写 retry result、替换 lease 并创建新 attempt。崩溃发生在远端请求发出后、本地 complete 前时，恢复方无法证明应 conflict，仍可能二次调用。必须冻结持久化的“请求已开始”CAS/阶段及 `rerun_checks` 特殊 claim/reclaim 事务，再给崩溃窗口 fixture。 |
| G5 effective policy | **已关闭** | active [`policy.md` §2–§4](../specs/policy.md) 冻结 `EffectivePolicyV1`、默认、范围、`>=` 风险边界、matcher、canonical hash 与资格收窄；Gate 只消费同一 closed shape，没有私有 policy 投影。 |
| G6 calibration / Interrupt identity | **已关闭** | active [`ledger.md` §3](../specs/ledger.md) 穷尽映射全部 Gate verdict 为 `allow|block|inconclusive`，storage enum 同步；[`gate.md` §5](../specs/gate.md#5-调用shadow-gate-与-hitl-事务) 与 [`interrupt.md` §5](../specs/interrupt.md#5-生成键与发布) 一致使用 immutable `effective_policy_hash/rule_id/matched_paths_digest`。guardrail golden vector 重算为 `ecc93a…111fb`，与文档一致，不再依赖不存在的 `policy_snapshot_id`。 |

## 3. P1 关闭条件

1. **G1：** 消除 `paths_complete=false` 的 schema/组装矛盾，冻结唯一入口、错误码和 fixtures；不得让实现方决定该对象是否能进入纯函数。
2. **G2：** 为 `change.state=closed|merged` 冻结互斥结果与判定优先级，或在 input contract 中以唯一 contract error 排除，并覆盖两个枚举值。
3. **G4：** 在 storage/outbox 冻结 durable request-start boundary，以及 `rerun_checks` 区别于通用 operation 的 claim、lease expiry、reclaim 和 complete CAS；测试“调用前崩溃”可重试、“调用中/调用后结果丢失”只 conflict 且不再调用。

## 4. 验收判断

- `gate.md` 转 `active`：**NO**
- P1 数量：**3**
- G1–G6 全关闭：**NO（G3/G5/G6 closed；G1/G2/G4 open）**
- 评审报告入库：**YES**
