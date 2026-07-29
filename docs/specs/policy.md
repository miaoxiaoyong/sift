---
status: active
created: 2026-07-29
summary: 项目策略字段、有效组装与漂移诊断契约
---

# Policy 规格

本文规定项目策略在 Gate **之外**的读取、closed 校验、有效组装、资格收窄和诊断边界，并冻结 V0 `.sift/policy.yaml` 的字段级 schema。Gate 的输入与判定顺序见 [`gate.md`](gate.md)。

来源：[PRD §5.4、§5.7](../PRD.md)、[DESIGN §8.4–§8.5、§9.4、§11](../DESIGN.md)、[WBS M4 §4.1](../WBS.md)。全局缺省和认证规则版本的唯一事实来源是 [`config.md` §3.11–§3.12](config.md)，认证投影见 [`ledger.md` §5](ledger.md)，持久化 Gate 输入字段见 [`storage.md` §10.2](storage.md)，远端 CAS capability 见 [`forge.md` §4.13](forge.md)。

字段级评审：[2026-07-29-policy-review-pi-gpt-5.6-sol.md](../reviews/2026-07-29-policy-review-pi-gpt-5.6-sol.md)。

## 1. 边界与不变量

1. 项目策略文件是该项目 base 分支的 `.sift/policy.yaml`；读取必须使用已解析为 commit SHA 的 base revision（`git show <base_sha>:.sift/policy.yaml`），而非符号分支名或 worktree 路径。代码中不存在读取 worktree policy 的入口。
2. YAML 必须先进入全局 decode gateway 的 `closed` 策略；未知字段、重复 key、非字符串 map key、多文档、alias 循环、错误类型、未知枚举和违反交叉约束均为 schema 失败。不得为 policy 另设宽松 YAML/JSON 解码器。
3. schema 失败只使该项目拒绝接入或隔离：停止其摄入与调度、写项目健康/一次告警并由 doctor 报 error；不得令健康项目停机，也不得静默用全局缺省替代坏文件。
4. 全局配置只提供缺省，项目 base policy 的显式值优先。项目 policy 不能声明认证阈值、认证版本、认证结果或 forge capability。
5. `auto_merge` 等提权不能由项目文件单独开启；组装器必须以资格事实单调收窄。资格不足时只关闭对应提权项，不能放宽其他规则。
6. Gate 是无 IO、无时钟的纯函数。它只消费已冻结的 `effective_policy`、认证版本及其他快照事实；不得读取配置、认证投影、forge capability、git 或 policy 文件。

## 2. 输入 schema v1

文件存在时，YAML 顶层必须是 closed object，`version` 必填且当前只能为整数 `1`；其余字段可省略。空文档、`null`、数组以及不含 `version` 的 `{}` 均非法。“没有项目显式覆盖”的合法空策略是：

```yaml
version: 1
```

完整字段如下：

```yaml
version: 1
protected_paths:
  hard: ["security/**"]
  soft: ["docs/api/**"]
  soft_exceptions: ["docs/api/generated/**"]
review_policy: risky-only
risky_review_threshold: 40
auto_merge: false
checks_pending_timeout: 1h
flaky_retry_limit: 1
```

| 字段 | 类型 | 省略语义 | 约束 |
|---|---|---|---|
| `version` | integer | 不可省略 | 只能为 `1` |
| `protected_paths` | closed object | 三个列表均为空 | 子字段均可省略；不接受 `null` |
| `protected_paths.hard` | string[] | `[]` | 项目追加的不可豁免规则；不能移除 §3.2 内建规则 |
| `protected_paths.soft` | string[] | `[]` | 命中后产生 `guardrail_violation` HITL 的规则 |
| `protected_paths.soft_exceptions` | string[] | `[]` | 仅取消匹配的 soft 命中；对任何 hard 规则无效 |
| `review_policy` | enum | 继承全局缺省 | `always \| risky-only \| never` |
| `risky_review_threshold` | integer | 继承全局缺省 | `0..100`；`risk_score >= threshold` 为高风险 |
| `auto_merge` | boolean | 继承全局缺省 | `true` 仍须经 §4.1 资格收窄 |
| `checks_pending_timeout` | duration string | 继承全局缺省 | `1m..24h`；Go duration 语法，不接受数值或负值 |
| `flaky_retry_limit` | integer | 继承全局缺省 | `0..10`；同一 Change `head_sha` 的 flaky Check 重试上限 |

`review_policy` 不为 `risky-only` 时，阈值不参与该次 review 判定，但仍是完整有效策略的一部分；禁止根据枚举值删除字段或产生两种 canonical shape。T3 不可用时按 [`brain.md` §9.3](brain.md) 固定为 100，因此在全部合法阈值下均要求 review。

### 2.1 path pattern 文法

每个 path pattern 是 1..1024 UTF-8 bytes 的仓库根相对 pattern。每个列表最多 256 项；同一列表规范化后重复即 schema 失败。pattern：

- 使用 `/` 分段；不得以 `/` 开头或结尾，不得含空段、反斜杠、NUL、`.` 或 `..` 段；不做 Unicode、大小写或 percent-decode 归一化；
- 普通字符按字节精确匹配；`?` 匹配单段内一个非 `/` 字节，`*` 匹配单段内零个或多个非 `/` 字节；
- 完整段 `**` 匹配零个或多个完整路径段；`**` 不得作为其他字符的一部分；V0 没有转义、字符类、否定 pattern 或 brace expansion；
- 匹配整条路径，而非子串。Forge 路径先统一为不带前导 `/` 的 `/` 分隔仓库相对路径；不能形成该表示的路径使 Gate 输入契约失败，不得猜测为未命中。

Change 的候选路径集合包含新增/修改/删除路径，rename 同时包含 old path 和 new path；先去重、按 UTF-8 bytes 排序，再匹配全部规则。任一候选路径命中任一 hard 规则即 hard；否则，命中 soft 且未命中任一 `soft_exceptions` 才是 soft。规则数组顺序不影响结果，hard 永远优先。

`soft_exceptions` 是 PRD “记住”动作最终形成的仓库策略表达，不是该动作本身。只有人显式发起的 policy Change 进入 base 后才生效；Gate 不直接写入该列表。宽泛 exception 可以取消多个项目 soft rule，这是可见的 base policy 变更，但永远不能取消内建或项目 hard rule。

## 3. 读取、规范化与有效策略

### 3.1 来源身份与失败

项目启用/启动探测按 [`config.md` §5.2](config.md) 验证 repo、base 分支和该 base SHA 的 policy。一次读取的身份至少包含 `project_id`、`base_sha`、文件存在性及原始 bytes 的 SHA-256；诊断说明项目与 revision，但不得输出文件内容或凭据。

文件在该 base SHA 中缺失是正常状态，规范化为 `{"version":1}` 后补全缺省；它不等同于空文档，也不等同于 schema 失败。base revision 不存在、对象不可读或 `git show` 失败是 repo/project error，不能伪装成文件缺失。运行期 Forge 返回 `AuthOrCapability` 时沿用项目隔离语义，不属于 Gate 输入读取错误，也不得退化为无条件 merge。

### 3.2 内建 hard rules

V0 内建、不可删除或降级的 hard rule 集为：

```json
[".github/workflows/**",".gitlab-ci.yml",".sift/**"]
```

它们分别覆盖 V0 两个 Forge 的 CI 定义入口和 Sift 自身策略/Context。新增受支持 Forge 或 CI 定义入口时，必须先版本化本 schema/Gate 语义并扩充该集合；不得让项目 policy 声称旧内建规则是 soft。项目 `protected_paths.hard` 与内建集合做 union。

### 3.3 组装顺序和 canonical shape

组装器是 Gate 外的确定性函数：

```text
assemble(basePolicy, gateDefaults, certificationProjection, forgeCapabilities)
  -> EffectivePolicyV1, effectivePolicyHash, certificationVersion, qualificationReport
```

顺序固定：

1. 对 base policy 做 v1 closed 校验和规范化；失败按 §3.1 隔离，不调用后续步骤。
2. 用启动期冻结的全局 `gate_defaults` 填充省略的五个 scalar；项目显式值优先。将内建 hard rules 与项目 hard rules union。
3. 对每个提权项应用资格谓词；不满足即从候选策略中关闭。该步骤幂等且不能被项目字段绕过。
4. 生成下列 closed `EffectivePolicyV1` 的 canonical JSON，并计算 SHA-256 小写十六进制 `effective_policy_hash`：

```json
{
  "schema_version": 1,
  "protected_paths": {"hard": [], "soft": [], "soft_exceptions": []},
  "review_policy": "always",
  "risky_review_threshold": 1,
  "auto_merge": false,
  "checks_pending_timeout_ms": 3600000,
  "flaky_retry_limit": 1
}
```

`EffectivePolicyV1` 是 Gate 消费的唯一 policy 类型：不得添加、删除或以别名替换上述字段。所有 path 列表按 UTF-8 bytes 排序且去重；duration 解析后以整数毫秒输出，禁止保留输入拼写。对象 key 词典序、JSON 数字和 UTF-8 编码沿用 [`config.md` §4](config.md)。该 shape 不携 source revision、显式/继承标记、资格原因或 capability 证据；这些属于组装记录/`qualificationReport`，不能影响 Gate 判定。

`qualificationReport` 是组装和 doctor 的解释性产物，不是 Gate 的旁路输入；对 `auto_merge` 的状态枚举固定为 `not_requested | task_kind_uncertified | forge_cas_unavailable | effective`。多个失败同时存在时按前述枚举顺序选择第一个资格失败，并可另列不改变判定的诊断；不得包含认证样本明细或单条 Run 的放行建议。

### 3.4 默认来源

全局 [`gate_defaults`](config.md#311-gate_defaults) 必须提供并规范化以下五项：`review_policy=always`、`risky_review_threshold=1`、`auto_merge=false`、`checks_pending_timeout=1h`、`flaky_retry_limit=1`。内建 hard rules 与空 soft/exception 集由本规格提供，不属于可覆盖的全局配置。

## 4. 资格收窄与冻结

### 4.1 `auto_merge`

当前唯一需要外部资格事实的提权项是 `auto_merge`。最终值为 true 当且仅当：

- 合并后的候选策略为 true；
- `certificationProjection.task_kind` 精确等于本次冻结任务类别，且 `certified=true`；
- 项目 Forge capability 已证明 `MergeChange` 支持 expected-head CAS。

认证行缺失、task kind 不匹配、投影版本不合法、探测未完成/过期/歧义、capability 缺失或运行期 `AuthOrCapability` 均视为 false。后两种不得尝试无条件 merge；探测与持久化见 [`forge.md` §2、§4.13](forge.md)。新增需外部资格的提权项，必须先在本规格声明单调收窄谓词、冻结输入、report 枚举和 doctor 呈现，才能加入 schema；不能复用 `auto_merge=true` 作为泛化授权。

### 4.2 certification version

[`config.md` §3.12](config.md) 的 `certification_rules_version` 只标识算法、窗口和阈值，不足以标识随样本变化的认证投影。组装器消费 [`ledger.md` §5](ledger.md) 输出的类别级 `certification_version`；该 version 必须同时承诺 rules version、task kind 和当前可重算证据版本。即使候选 `auto_merge=false` 或收窄前后 effective bytes 相同，投影 version 变化仍必须进入 Gate 输入，使旧 cache 不可命中。

每次 Gate 调用前，reconciler 取得一次组装结果，将完整 `effective_policy`、`effective_policy_hash` 和该类别的 `certification_version` 写入规范化 Gate 输入快照。`gate_input_hash` 覆盖整份快照；hash/version 写入 [`storage.md` §10.2](storage.md) 的同一不可变记录，缓存键仍仅为 `(gate_input_hash, gate_version)`。

快照取得后，base policy、全局配置、认证投影或 Forge capability 的变化不得改写该次 verdict；后续调用重新读取 base SHA、组装并取得新快照。缓存和回放只引用冻结快照，不回读当前策略事实。

## 5. `sift doctor` 漂移

每个已配置项目必须显示：base SHA、文件状态 `missing | valid | invalid`、项目健康/隔离状态、显式 scalar overrides、三类 path 规则、`effective_policy_hash`、`certification_version` 和 `auto_merge` 资格状态。不得输出 policy bytes、认证样本或 capability 探测原文。

严重度固定如下：

| 情况 | 严重度 |
|---|---|
| policy/repo schema 或读取失败、项目隔离、Forge auth/capability 失效 | error |
| 任一项目 scalar 显式值不同于全局缺省，或声明任一 path rule | warning（显式漂移，不表示错误） |
| 请求 `auto_merge` 但被认证或 CAS 资格收窄 | warning |
| 两个有效项目的 effective policy 不同，但差异已由上列 warning 解释 | info 横向对比 |
| policy 缺失或合法空策略，且有效值等于全局缺省加内建 hard rules | ok |

显式漂移按 base policy 的“字段是否出现”判断：未声明、仅由缺省填入的字段不能标为显式偏离；显式写出与全局相同的值显示为 explicit/same，但不是 warning。effective 横向比较使用本规格的完整 canonical shape，不比较 map/string 展示文本。被资格收窄必须显示“requested but ineffective”，不能冒充项目从未配置，也不能解释为 Gate 已作出裁定。

离线 doctor 只能按 [`config.md` §7](config.md) 的只读限制使用可获得的文件和 SQLite 投影；不得写健康状态、迁移数据库、探测 Forge 或重算会改变 daemon 有效状态的认证投影。缺少在线投影时显示 unknown，并按其已有持久化健康事实定级，不猜测资格有效。

## 6. 验收派生

- closed vectors 覆盖：缺失文件、`version: 1`、空文档、未知/重复字段、错误 scalar 类型、非法 duration、非法/重复 pattern、数组上限和全部交叉边界。
- path vectors 覆盖 `*`/`?`/`**`、根文件、rename old/new、删除、hard/soft 同时命中、soft exception，以及 exception 不能取消内建或项目 hard。
- 全局缺省与项目显式值逐字段合并；path 集排序、duration 毫秒化和 canonical hash 在输入顺序/拼写变化下稳定。
- `auto_merge` 的配置、类别认证、projection version 与 CAS capability 缺任一均为 false；资格变化改变冻结输入，旧 cache miss。
- 坏 policy 只隔离单项目；doctor 对 missing、invalid、显式漂移、横向差异和 requested-but-ineffective 给出确定级别。
