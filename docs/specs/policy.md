---
status: draft
created: 2026-07-29
summary: 项目策略校验、有效策略组装与漂移诊断契约
---

# Policy 规格

本文规定项目策略在 Gate **之外**的读取、校验、组装、资格收窄和诊断边界。Gate 的输入/判定顺序与字段级策略评审分别属于后续的 `specs/gate.md` 和独立字段评审；本文不以草案字段替代两者。

来源：[PRD §5.4、§5.7](../PRD.md)、[DESIGN §8.4–§8.5、§9.4、§11](../DESIGN.md)、[WBS M4 §4.1](../WBS.md)。全局缺省和认证版本的唯一事实来源是 [`config.md` §3.11–§3.12](config.md)，持久化 Gate 输入字段见 [`storage.md` §10.2](storage.md)，远端 CAS capability 见 [`forge.md` §4.13](forge.md)。

## 1. 边界与不变量

1. 项目策略文件是该项目 base 分支的 `.sift/policy.yaml`；读取必须使用 base revision（`git show <base>:.sift/policy.yaml`）而非 worktree 路径。不存在读取 worktree 内 policy 的实现入口。
2. YAML 必须先进入全局 decode gateway 的 `closed` 策略；未知字段、重复 key、错误类型、未知枚举和违反交叉约束均为 schema 失败。不得为 policy 另设宽松 YAML/JSON 解码器。
3. schema 失败只使该项目拒绝接入或隔离：停止其摄入与调度、写项目健康/一次告警并由 doctor 报 error；不得令健康项目停机，也不得静默用全局缺省替代坏文件。
4. 全局配置只提供缺省，项目 base policy 的显式值优先。项目 policy 不能声明认证阈值、认证版本或 forge capability。
5. `auto_merge` 等提权不是项目文件可单独开启的能力；组装器必须以资格事实收窄它们。资格不足时只能关闭对应提权项，不能放宽任何其他规则。
6. Gate 是无 IO、无时钟的纯函数。它只能消费已冻结的 `effectivePolicy`、认证版本及其他快照事实；不得读取配置、认证投影、forge capability、git 文件或 policy 文件。

项目 policy 的字段集合包含 PRD 所列的护栏、审阅、自动合并、超时与重试策略；其中可由全局给缺省并由本草案消费的字段以 [`config.md` §3.11](config.md) 为准。字段名、类型、默认 hard guardrail 集及逐字段合并/校验规则不在本草案冻结，须经独立字段级评审后写入本文件与 `specs/gate.md`，并同步生成 closed schema。该评审完成前，未被批准的字段不得实现为可接受输入。

## 2. 项目级校验与来源

项目启用/启动探测按 [`config.md` §5.2](config.md) 验证 repo、base 分支及该 base revision 的 policy schema。校验对象的身份至少包括 project id、base revision 与 policy bytes digest；诊断必须能说明失败发生在哪个项目和 revision，且不得泄露文件内容或凭据。

合法的空 policy 与缺失文件都表示“没有项目显式覆盖”：缺失文件在 decode 前规范化为空对象，再由全局缺省补齐；它们不等同于 schema 失败。其 JSON 表示必须由字段级 schema 固定，不能由调用点猜测。

运行期 forge 返回 `AuthOrCapability` 时，沿用项目级隔离语义。它不是 Gate 的输入读取错误，也不得退化为无条件 merge。

## 3. 有效策略组装

组装器是 Gate 外的确定性函数，接收已验证的输入：

```text
assemble(basePolicy, gateDefaults, certification(taskKind, version), forgeCapabilities)
  -> effectivePolicy, effectivePolicyHash, certificationVersion, qualificationReport
```

顺序固定：

1. 对 base policy 进行 closed schema 校验并规范化；失败按 §2 隔离，**不**调用后续步骤。
2. 用全局 `gate_defaults` 填充项目未声明的可覆盖项；项目显式值不得被全局值覆盖。
3. 对每个声明为提权的项应用其资格谓词，未满足则从合并后的候选策略中剔除/关闭。资格收窄必须是幂等的，且不能由该项目 policy 绕过。
4. 对最终有效策略生成 canonical JSON 和 SHA-256 小写十六进制 `effective_policy_hash`。canonical JSON 的通用编码规则沿用 [`config.md` §4](config.md)。

`qualificationReport` 是组装/doctor 的解释性产物，不是 Gate 的旁路输入；至少区分“未配置”“配置但类别未认证”“配置但 forge CAS capability 不可用”“有效”。它不得包含认证样本明细或单条 Run 的放行建议。

### 3.1 `auto_merge` 的资格谓词

当前唯一已定义的提权项为 `auto_merge`。最终值为 true 当且仅当以下三项同时为真：

- 合并后的候选策略配置了 `auto_merge`；
- 当前任务类别在当前 `certification_version` 下的认证投影为 `certified=true`；
- 该项目的 forge capability 已证明远端 `MergeChange` 支持 expected-head CAS。

认证行缺失、版本不匹配、探测未完成/过期/歧义、capability 缺失或运行期返回 `AuthOrCapability` 均视为 false。后两种不得尝试无条件 merge；具体探测与持久化规则以 [`forge.md` §2、§4.13](forge.md) 为准。

新增提权项必须先在本规格声明其单调收窄资格谓词、冻结输入与 doctor 呈现，才可加入 schema。不能复用 `auto_merge=true` 作为泛化授权。

## 4. 冻结与 Gate 边界

每次 Gate 调用前，reconciler 取得一次组装结果并把最终有效策略、`effective_policy_hash` 和 `certification_version` 放入完整规范化 Gate 输入快照。`gate_input_hash` 覆盖整份快照；`effective_policy_hash` 与 `certification_version` 必须写入 [`storage.md` §10.2](storage.md) 的同一不可变记录。

`certification_version` 按 [`config.md` §3.12](config.md) 从算法版本和 canonical certification 配置得出。即使资格收窄前后有效策略字节恰好相同，认证版本变化仍是不同的冻结输入，必须使旧 Gate cache 不可命中。缓存键仍仅为 `(gate_input_hash, gate_version)`。

在 Gate 已取得快照后，base policy、全局配置、认证投影或 forge capability 的任何变化不得改写该次 verdict；后续调用重新组装并取得新快照。影子记录、缓存和回放均只引用该冻结快照，不回读当前策略事实。

## 5. `sift doctor` 策略漂移

doctor 在 Gate 外执行横向诊断。对每个已配置项目，它应显示：base policy 的校验/隔离状态、相对全局 `gate_defaults` 的显式偏离项、规范化有效策略 hash、认证版本和提权资格结果。横向比较以字段级 schema 中可比较的策略字段为集合；不把项目未声明、仅由缺省填入的字段错误标为偏离。

doctor 必须将以下情况分别呈现：schema 失败（error）、项目隔离或 capability/auth 失效（error）、有效策略不同于全局缺省或项目之间不同（warning/信息性漂移，严重度由 doctor 总体规则决定）。它不得把“被资格收窄为 false”显示成“项目从未配置”，也不得把策略漂移解释为 Gate 已作出裁定。

离线 doctor 只能按 [`config.md` §7](config.md) 的只读限制使用可获得的文件/SQLite 投影；不得写健康状态、迁移数据库、探测 forge 或重算会改变 daemon 有效状态的策略。

## 6. M4 验收映射

| WBS M4 §4.1 判据 | 本规格判定 |
|---|---|
| closed 校验失败只隔离该项目 | §1–§2 的项目级失败路径，无 defaults 兜底 |
| Gate 外组装与资格剔除 | §3 的固定顺序与单调收窄 |
| `auto_merge` 三重条件 | §3.1；缺任一条件均为 false |
| hash/version 进入冻结输入 | §4 与 `storage.md` §10.2 |
| doctor 横向漂移，Gate 不读外部事实 | §1、§5 |

## 7. 自查结果

- [x] 项目 policy closed 校验失败只隔离该项目。
- [x] 定义 base policy、全局缺省、认证和 forge capability 的 Gate 外组装顺序。
- [x] `auto_merge` 要求配置、类别认证与远端 expected-head CAS capability。
- [x] `effective_policy_hash` 和 `certification_version` 均进入冻结输入。
- [x] doctor 漂移与 Gate 纯函数边界明确。
- [x] 字段级 schema 保留给独立评审，未把未批准字段变为实现契约。
