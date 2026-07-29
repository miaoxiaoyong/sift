PASS WITH NOTES

# policy.md 字段级评审

> 日期：2026-07-29
> 评审人：pi × GPT-5.6-sol
> 评审对象：[`docs/specs/policy.md`](../specs/policy.md) draft（PR #213）
> 依据：[PRD §5.4、§5.7、§9.1](../PRD.md)、[DESIGN §8.4–§8.5、§9.4](../DESIGN.md)、[WBS M4 §4.1](../WBS.md)
> 交叉核对：[`config.md`](../specs/config.md) §3.11–§3.12、[`gate.md`](../specs/gate.md)、[`ledger.md`](../specs/ledger.md) §5、[`storage.md`](../specs/storage.md) §10.2/§10.6、[`forge.md`](../specs/forge.md) §4.13

## 1. 结论

**PASS WITH NOTES。** PR #213 的组装方向正确，但原稿明确把字段集留给本次评审，因而尚不能直接生成 closed schema 或确定 Gate 输入。本 worktree 已关闭全部 P1/P2：冻结 policy v1 输入、path pattern 与优先级、完整 effective shape、`risky-only` 阈值、认证规则版本/投影版本边界以及 doctor 定级。没有遗留 P1，`policy.md` 已转为 `active`。

本结论只表示 Policy 字段契约可进入 M4 实现，不表示 Gate、Ledger、policy loader/schema 生成或 V5b/V6 已实现，也不替代 #215/#217 的独立字段评审。

## 2. 发现与核销

| 项 | 级别 | 发现 | 处置 |
|---|---|---|---|
| P1 | P1 | 原 §1 明示字段名、类型、默认 hard guardrail、逐字段合并规则均未冻结，却在 §7 自查中把 Policy 规格写作已完成；两个实现无法生成同一 closed schema。 | 在 `policy.md` §2 冻结 v1 顶层 closed object、required/optional、类型、枚举、范围、duration 和空/缺失文件语义；未批准字段继续由 unknown-field 拒绝。 |
| P2 | P1 | `protected_paths` 没有 pattern 文法、hard/soft 冲突优先级、rename/delete 候选路径或 remembered exception 的字段表达；默认 hard 集也只有 PRD 链接，没有进入 effective shape。 | 冻结 `/` 分段的 `*`/`?`/完整段 `**` 文法、输入上限、old/new path 集、hard 优先、`soft_exceptions` 及 V0 三条内建 hard rule；exception 永远不能取消 hard。 |
| P3 | P1 | Gate 要比较“有效策略阈值”，但 policy/config 均无阈值字段；`review_policy=risky-only` 对 0..100 T3 分数没有确定边界。 | 新增 `risky_review_threshold: 0..100`，全局保守缺省为 `1`，比较固定为 `risk_score >= threshold`；T3 fallback=100 在全部合法阈值下均要求 review。同步修正 `config.md` 与 `gate.md`。 |
| P4 | P1 | 原稿把 `certification_version` 说成仅由算法+配置得出；Ledger 又要求它随当前证据版本变化。同名不同义会让样本增长后的资格投影无法被 version 唯一承诺。 | 将 config 产物明确为 `certification_rules_version`；类别级 `certification_version` 同时承诺 rules version、task kind 与当前可重算证据版本。同步收紧 `storage.md` 字段说明。 |
| P5 | P2 | “合法空 policy”和缺失文件都规范化为空对象，但未说明文件存在时是否要 version；空 YAML、`{}`、git object 不可读可被不同实现当作缺失。 | 文件存在时必须为含 `version: 1` 的 object；只有 base SHA 中路径确实缺失才规范化为该最小对象。空文档/`{}` 失败，base revision/read 失败是 repo error。 |
| P6 | P2 | effective policy 只规定“canonical JSON”，未冻结字段名、duration 单位、数组顺序和解释性资格字段是否入 hash。 | 冻结 closed effective shape；path set 按 bytes 排序、duration 转整数毫秒；source/qualification report 不入 effective bytes，避免展示字段改变 verdict hash。 |
| P7 | P2 | doctor “warning/信息性漂移，严重度由总体规则决定”不是可测试契约；也未区分显式覆盖、继承和请求后被资格收窄。 | 固定 error/warning/info/ok 表、explicit/same 语义、横向比较对象及离线 unknown 边界。 |

## 3. 字段级核对

- **输入闭包**：`version`、三类 path rule、review policy/threshold、auto merge、Checks pending timeout、flaky retry limit 均有类型、省略语义和范围。
- **安全护栏**：内建 hard 只能 union；hard 胜过 soft/exception；`.sift/**` policy Change 仍按旧 base policy 受审。
- **确定匹配**：pattern 不依赖 shell、OS path separator 或隐式 gitignore 库；rename 两端与 delete 都不能漏检。
- **有效组装**：五个 scalar 逐字段 overlay；资格阶段只单调关闭 `auto_merge`；effective canonical shape 可直接参与 hash/replay。
- **版本边界**：规则配置 version 与随证据演进的类别投影 version 分离；最终 projection version 与 effective hash 均进入冻结 Gate 输入。
- **诊断**：坏项目隔离不拖停健康项目；显式漂移、横向差异与 requested-but-ineffective 不再混成同一状态。

## 4. 必要交叉修正

- [`config.md`](../specs/config.md) §3.11：补 `risky_review_threshold=1`；§3.12 将规则配置摘要命名为 `certification_rules_version`，避免冒充完整投影版本。
- [`gate.md`](../specs/gate.md) §3：把 risky 比较固定到 `effectivePolicy.risky_review_threshold` 与 `>=`。
- [`storage.md`](../specs/storage.md) §10.6：`certification_version` 字段说明补 task kind 与当前证据版本。

以上均是让 Policy 字段可被现有交叉契约消费的必要修正；未修改 PRD、DESIGN、WBS、实现或其他无关文件。

## 5. 非阻断注记

1. `risky_review_threshold=1` 是保守缺省：只有 T3 明确给 0 才可在 `risky-only` 下免审。它不改变 PRD 已结案的默认 `review_policy=always`；后续如需调宽，必须作为显式 policy/config 变更审计。
2. V0 内建 CI hard 集只覆盖当前两种 Forge 的入口：`.github/workflows/**` 与 `.gitlab-ci.yml`。未来支持其他 Forge/CI 入口时须版本化扩表，不能依赖“等价配置”自然语言。
3. `soft_exceptions` 只定义进入 base 后的策略表达；人发起 remembered-policy Change 的 Command/Forge 流程仍属 M5，Gate #215 只需保证当前 head 的一次性批准不被误写成该字段。
4. active config 的 Go raw/effective struct 尚无新阈值字段；这是 M4 Policy 实现必须同步的 schema/code 工作，不属于本 docs-only 评审通过或实现完成的证据。

## 6. 验收判断

- 评审报告入库：**YES**
- P1 遗留：**0**
- `policy.md` 转 `active`：**YES**
- M4 Policy/Gate 实现完成：**NO**
- V5b/V6 门禁通过：**NO**
