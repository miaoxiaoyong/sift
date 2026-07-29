---
status: active
created: 2026-07-29
summary: Forge 适配层的最小动词集签名、中性类型、平台归一、actor 契约与错误分类
---

# Forge 适配层规格

本文冻结 Forge 适配层的端口契约：PRD §5.2 最小动词集（完整签名与中性类型）、CI rerun、平台归一规则、actor 必填、Change marker 全状态查找、merge expected-head CAS、错误分类、argv 边界与 API 预算收费口。字段级审定记录见[评审报告](../reviews/2026-07-29-forge-review-pi-gpt-5.6-sol.md)。

来源：[PRD §5.2、§5.4、§9.2](../PRD.md)、[DESIGN §8.1](../DESIGN.md)、[WBS M2 §2.1–§2.5](../WBS.md)。现存 M1 fake 骨架（[`internal/forge/forge.go`](../../internal/forge/forge.go)）只定义了 `Kind`、`ProjectRef`、不完整的中性投影与 `Client` 的三个骨架动词（`ListIssuesByLabel`、`ListLabelEvents`、`GetChange`）。本文冻结 PRD 所列 13 个动词及 Gate 所需的 `RerunCheck`、完整中性类型、双平台归一细节与副作用对账端口，是 M2/M4 实现的共同契约。M2 必须按本文一次性升级 M1 骨架与 fake；不得把 M1 为骨架链刻意缩小的签名误当成已冻结的 M2 端口。

## 评审处置

2026-07-29 字段级评审结论为 **PASS**。审定修订闭合了目标类型歧义、Issue/Change 必需事实、游标推进、分页收费、标签并发覆盖、GitLab merge method 与预算存储口径；逐项见[评审报告](../reviews/2026-07-29-forge-review-pi-gpt-5.6-sol.md)。

## 1. 不变量

1. `gh api` / `glab api` 是唯一 forge 通道；verb 优先使用 `api` 子命令（plumbing）而非 porcelain。任何动词不得绕过 CLI 直调 HTTP。
2. 平台字段（`number`/`iid`、`mergeable_state`/`detailed_merge_status`、Checks vs Pipelines、Draft 前缀）在边界归一为领域中性类型，不得泄漏到上层。
3. 两平台无法都给出确定性答案时，归一结果是显式 `unknown`，由上层转 HITL——**适配器不得猜测**。
4. actor 是类型的一部分：`ListLabelEvents`、`ListIssueComments`、`ListChangeComments` 返回类型中 actor 为必填字段；取不到 actor 的驱动性事件在适配器内即**丢弃**（fail closed，DESIGN §8.1 / PRD §9.2）。
5. 进程调用一律 argv 数组启动，**禁止 shell 拼接**。
6. 错误分类只暴露五种语义：`Transient` | `RateLimited` | `AuthOrCapability` | `ContractViolation` | `SemanticConflict`。平台 HTTP 状态码 / 退出码 / stderr 细节锁定在适配器内部。
7. API 预算只在 Forge 适配层收费；上层不感知 `gh`/`glab` 的速率限制细节。
8. 副作用对账是端口能力，不是上层自行查询：`FindChangeForCreateOperation`（跨全状态 marker 搜索与冲突检测）、`MergeChange`（expected-head CAS）由适配器实现，outbox worker 不得用 raw API 旁路。
9. 自动合并能力只探测**已配置项目实际引用到的** forge，且在任何 merge worker 启动前执行。探测把 `capabilities_json.auto_merge` 与检查时间、审计事件一同持久化；初值、缺字段、探测歧义和重启后的未重探状态一律为 `false`。探测只验证 CLI 能以 request body 提交 expected-head CAS 字段，禁止用首次真实 merge 当探测；merge 路径必须同时消费本进程探测结果和持久化项目状态。
10. 所有动词签名以本规格为准。M1 fake 的三个骨架动词中，`ListLabelEvents` 尚缺 `TargetRef` 与返回游标，`Issue`/`Change` 投影也缺 M2 必需事实；M2 扩展 fake 时必须同步迁移。

## 2. 基础类型

以下为 M2 完整类型；标注为骨架已有的类型也必须按本节补齐字段与校验：

### `Kind`
```
"github" | "gitlab"
```
Forge 平台族。能力探测按此枚举分支。

### `ProjectRef`
```
Kind       Kind
Host       string   // 规范化主机名，如 github.com
ProjectKey string   // 如 org/repo
```
唯一标识一个已配置的 forge 项目。

### `TargetKind` / `TargetRef`
```
TargetKind = "issue" | "change"

Kind TargetKind
ID   string     // 项目内 number / iid
```
明确区分 Issue 与 Change。GitLab 两类对象可有相同 `iid` 且端点不同，任何目标型动词不得只收一个裸 `targetID`。

### `IssueState`
```
"open" | "closed"
```

### `Issue`
```
ID     string
Title  string
Body   string
Author string     // 必填；缺失为 ContractViolation
URL    string
State  IssueState
Labels []string   // 适配器排序去重
```
领域中性 Issue 投影。`State` 是 PRD §4.5 关闭事实的观测口；`Labels` 由适配器排序去重。

### `LabelAction`
```
"added" | "removed"
```

### `LabelEvent`
```
TargetID   string      // issue 或 change id
Label      string
Action     LabelAction
Actor      string      // 必填；缺失则整条事件在适配器内丢弃
ObservedAt time.Time
```
actor 取不到时整条事件丢弃——调用方永远不会看到空 Actor 的 `LabelEvent`。

### `ChangeState`
```
"open" | "merged" | "closed"
```
三个状态是两平台可合并性语义的保守交集；`mergeable_state` / `detailed_merge_status` 这类平台特有字符串永不透出。

### `Mergeability`
```
"mergeable" | "conflicting" | "unknown"
```

### `ReviewState`
```
"approved" | "not_approved" | "unknown"
```

### `Change`
```
ID           string
URL          string
HeadSHA      string
State        ChangeState
Mergeability Mergeability
ReviewState  ReviewState
IsDraft      bool
MergedAt     time.Time   // 仅 State=merged 时非零
```
领域中性 Change 投影。`HeadSHA` 是 merge 的 expected-head 契约锚点；`URL` 是 outbox 成功证据所需字段。平台不能确定可合并性或审查状态时必须返回 `unknown`，不得用零值伪装确定结论。草稿状态在两平台都可确定，必需字段缺失为 `ContractViolation`。

### `Cursor`
```
string  // 适配器不透明游标
```
增量游标。上层只保存、透传，不解析内容。适配器必须处理同时间戳多条记录：游标至少编码时间边界与稳定 tie-breaker，下一次允许重放但不得跳项；调用方按远端事件/评论 ID 幂等。空游标表示从可查询历史起点开始。

## 3. 错误分类

已由 M1 定义（[`internal/forge/forge.go`](../../internal/forge/forge.go)），本文冻结为唯一暴露面：

| Sentinel | 语义 | 上层动作 |
|----------|------|---------|
| `ErrTransient` | 网络抖动 / 临时不可用 | 指数退避重试 |
| `ErrRateLimited` | 速率限制，含远端 reset 时间 | 尊重 `Retry-After`/`RateLimit-Reset` 并联动 API 预算降速 |
| `ErrAuthOrCapability` | 凭证失效 / 权限不足 | 停止该项目摄入 + 产生一次告警，不循环轰炸 |
| `ErrContractViolation` | 响应结构不合预期（必填字段缺失、类型错误） | 保留响应摘要，fail closed |
| `ErrSemanticConflict` | 语义冲突（Change marker 未命中而同 base/head 有冲突、merge stale head 等） | 重读事实源后重判或转 HITL |

所有错误经 `ClassifiedError` 包装：
```
Class   error      // 上述 sentinel 之一
Summary string     // 适配器诊断摘要（不含凭证）
RetryAt time.Time  // 仅 RateLimited 可非零；远端未给可信 reset 时为零
```

`ClassifiedError` 实现 `Unwrap()`，上层用 `errors.Is` 分类。`RetryAt` 是退避/预算联动的结构化输入，不得要求上层解析 `Summary`。

## 4. 最小动词集（完整 13 个）

以下为 PRD §5.2 最小动词集的完整签名。每个动词注明对接的 `gh` / `glab` 端点和归一要点。M1 fake 的三个骨架动词在此纳入完整端口；其中两处签名/投影迁移已在 §11 明示。

### 4.1 `ListIssuesByLabel`
```
ListIssuesByLabel(ctx, project, label, since Cursor) → ([]Issue, Cursor, error)
```
**用途**：增量摄入。

**归一要点**：
- GitHub：`gh api /repos/{org}/{repo}/issues?labels={label}&since={since}&sort=updated&direction=asc&state=open`。只取 `pull_request` 为 null 的条目（排除 PR，Issue 与 PR 在 GitHub API 中共用编号空间）。
- GitLab：`glab api projects/{id}/issues?labels={label}&updated_after={since}&order_by=updated_at&sort=asc&state=opened`。
- 游标：规范化为适配器不透明 string。GitHub 用 `since`、GitLab 用 `updated_after` 缩小扫描范围；游标同时保留时间边界和稳定 ID tie-breaker。边界时间采用重叠读取并按 ID 去重，禁止仅保存最后一条 `updated_at` 而漏掉同时间戳记录。游标的解析仅发生在适配器内。
- Labels：适配器排序去重。
- Author：取自 `user.login`（GitHub）/ `author.username`（GitLab），缺失即 `ContractViolation`。
- 游标只在一批全部持久化后推进（由 Intake 层保证，适配器不负责）。

### 4.2 `GetIssue`
```
GetIssue(ctx, project, issueID string) → (Issue, error)
```
**用途**：读取单个 Issue 详情与正文。

**归一要点**：
- GitHub：`gh api /repos/{org}/{repo}/issues/{number}`。
- GitLab：`glab api projects/{id}/issues/{iid}`。
- 注意：GitLab 使用 `iid`（项目内编号），不是全局 `id`。适配器以 `iid` 作为 `issueID`；调用方传入的是项目内编号。
- Body 保留原始 markdown，不做平台差异清洗。
- `state` 映射为 `IssueState`；这是 PRD §4.5「Issue 被关闭」的事实观测口。

### 4.3 `ListIssueComments`
```
ListIssueComments(ctx, project, issueID string, since Cursor) → ([]Comment, Cursor, error)
```
**用途**：读取审批指令与人工补充（含 `/sift *` 指令）。**必须返回 `author`**。

**归一要点**：
- GitHub：`gh api /repos/{org}/{repo}/issues/{number}/comments?since={since}&sort=created&direction=asc`。
- GitLab：`glab api projects/{id}/issues/{iid}/notes?sort=asc&order_by=created_at`（按 `created_at` 过滤）。
- Actor：取自 `user.login`（GitHub）/ `author.username`（GitLab）。**缺失则丢弃该条评论**——这是 C8 的 fail closed 实现点之一（不影响同批次其他评论）。
- 增量游标语义同 `ListIssuesByLabel`；必须穷尽远端分页后才返回新游标。

### 4.4 `ListLabelEvents`
```
ListLabelEvents(ctx, project, target TargetRef, since Cursor) → ([]LabelEvent, Cursor, error)
```
**用途**：读取标签增删事件及其 actor——§9.2 的闸门依赖它。M1 只有无目标类型/返回游标的骨架。

**归一要点**：
- GitHub：Issue 与 Change 共用 `/repos/{org}/{repo}/issues/{number}/timeline`（按 `labeled`/`unlabeled` 事件过滤），或 `/issues/{number}/events`。
- GitLab：按 `target.Kind` 选择 `projects/{id}/issues/{iid}/resource_label_events` 或 `projects/{id}/merge_requests/{iid}/resource_label_events`。
- Actor：GitHub `actor.login` / GitLab `user.username`。**缺失则丢弃整条事件**。
- 返回游标必须覆盖本次已扫描的完整分页；没有返回游标就无法在 `Cursor` 不透明约束下安全推进。

### 4.5 `CommentTarget`
```
CommentTarget(ctx, project, target TargetRef, body string) → (commentID string, error)
```
**用途**：向 Issue 或 Change 发送决策简报 / 确认回执 / 告警评论。

**归一要点**：
- GitHub：两类目标均用 `gh api /repos/{org}/{repo}/issues/{number}/comments`。
- GitLab：按 `target.Kind` 选择 `glab api projects/{id}/issues/{iid}/notes` 或 `projects/{id}/merge_requests/{iid}/notes`。
- 返回远端 comment/note ID 供幂等对账（marker 搜索的 fallback）。
- comment body 内嵌 [`outbox.md` §5](outbox.md) 定义的不可见 operation marker；由 outbox worker 生成并注入，适配器只透传。Intake 等无 Run 评论不得伪造 `run_id`。

### 4.6 `SetLabels`
```
SetLabels(ctx, project, target TargetRef, add, remove []string) → error
```
**用途**：Issue / Change 的状态投影与触发、审批标签管理。

**归一要点**：
- GitHub：两类目标共用 Issue labels API；用 add-labels 与逐项 remove-label 端点，不做读后整集覆盖。
- GitLab：按 `target.Kind` 选择 Issue / Merge Request update 端点，并使用 `add_labels` / `remove_labels`。
- 禁止 read-modify-write 后覆盖整个集合；否则并发加入的人类标签会被静默删除。执行后重读，确认目标 add/remove 子集，同时保留无关标签。
- add 和 remove 先排序去重且不得相交；重复 add/remove 视为成功，保持 set 幂等语义（DESIGN §6.4）。

### 4.7 `CreateChange`
```
CreateChange(ctx, project, branch, base, title, body string) → (Change, error)
```
**用途**：**由 Sift 创建 Change**（PRD §5.1——Agent 不创建 Change）。成功后将远端 Change ID 返回并持久化。

**归一要点**：
- GitHub：`gh api /repos/{org}/{repo}/pulls -f head='...' -f base='...' -f title='...' -f body='...'`。返回 `number`、`html_url`、`head.sha` 与归一状态；创建参数明确 `draft=false`。
- GitLab：`glab api projects/{id}/merge_requests -f source_branch='...' -f target_branch='...' -f title='...' -f description='...'`。返回 `iid`、`web_url`、`diff_refs.head_sha` 与归一状态。若创建响应尚无 head SHA，适配器必须按 ID 重读；仍缺失则 `ContractViolation`。
- body 内嵌 `op_key` marker，与评论 marker 同机制、同一实现。
- 创建前**不检查**同 base/head 是否已存在 Change——那是 `FindChangeForCreateOperation` 的职责，outbox worker 先查后建。

### 4.8 `FindChangeForCreateOperation`
```
FindChangeForCreateOperation(ctx, project, opKey, payloadDigest, branch, base string) → (*Change, FindResult, error)
```
**用途**：为创建操作做崩溃对账——跨开启 / 关闭 / 已合并状态查找 operation marker，并返回同 base/head 的无 marker 冲突。**这是端口能力，outbox worker 不得绕过**（DESIGN §6.4）。

**`FindResult` 枚举**：
```
"marker_hit"       // marker 命中，返回该 Change
"no_match"        // marker 未命中，且同 base/head 无冲突
"semantic_conflict" // marker 未命中，但同 base/head 存在无 marker 的 Change
```

**归一要点**：
- marker 搜索：对开启 / 关闭 / 已合并的 Change 列表按 body 精确搜索由 `op_key` 和 `payload_digest` 共同生成的 marker。GitHub 用 `/pulls?state=all&head={owner}:{branch}&base={base}`；GitLab 用 `/merge_requests?state=all&source_branch={branch}&target_branch={base}`。所有页必须穷尽后才能判 `no_match`。
- marker 唯一命中才返回 `marker_hit`；命中多个对象为 `ErrSemanticConflict`，不得任选一个。
- 同 base/head 冲突：任何状态下的 Change，若 body 不含本次 `op_key` marker，即判 `semantic_conflict`（DESIGN §6.4「绝不接管他人对象」）。**适配器只返回结果，不裁决——裁判规则在上层**。
- marker 命中但 Change 已关闭或已合并：仍返回该 Change（不创建新的），上层按 PRD §4.5 收敛为 forge 外部事实；同 key 而 digest 不符不命中，不能接管。

### 4.9 `GetChange`
```
GetChange(ctx, project, changeID string) → (Change, error)
```
**用途**：读取 Change 状态、可合并性、head sha、审查状态与草稿状态。M1 仅实现了状态/head 骨架；M2 必须补齐投影。

**归一要点**：
- GitHub：`gh api /repos/{org}/{repo}/pulls/{number}`。`id` → `number`、`state` → `"open"/"closed"`、`merged_at` 非 null → `ChangeMerged`、`head.sha` → `HeadSHA`。
- GitLab：`glab api projects/{id}/merge_requests/{iid}`。`iid` → `ID`、`state` → `"opened"/"closed"/"merged"`、`merged_at` → `MergedAt`、`sha`（来自 `diff_refs.head_sha`）→ `HeadSHA`。
- GitHub 将 `mergeable`/`mergeable_state` 归一为 `Mergeability`，Reviews 归一为 `ReviewState`，`draft` 归一为 `IsDraft`；GitLab 将 `merge_status`/`detailed_merge_status`、Approvals 能力与标题 `Draft:`/`WIP:` 前缀归一到相同字段。
- GitHub `mergeable=null`、GitLab 状态仍在计算或平台/套餐无 Approvals 能力时返回对应 `unknown`。缺能力不是猜成通过；调用方据此转 HITL。

### 4.10 `GetChangeDiff`
```
GetChangeDiff(ctx, project, changeID string) → (string, error)
```
**用途**：供 T3 风险评分（LLM 读 diff）。

**归一要点**：
- GitHub：`gh api /repos/{org}/{repo}/pulls/{number} -H "Accept: application/vnd.github.v3.diff"` 返回 unified diff。
- GitLab：`glab api projects/{id}/merge_requests/{iid}/changes` 中取 `changes[].diff` 拼接，或 `GET .../merge_requests/{iid}.diff`。
- 返回原始 unified diff 字符串。不做平台差异清洗（T3 的 prompt 已声明输入来源于 GitHub/GitLab）。

### 4.11 `ListChangeComments`
```
ListChangeComments(ctx, project, changeID string, since Cursor) → ([]Comment, Cursor, error)
```
**用途**：读取审批指令。**必须返回 `author`**。

**归一要点**：
- GitHub：`gh api /repos/{org}/{repo}/issues/{number}/comments?since={since}&sort=created&direction=asc`，只拉 PR 对话区的 issue-style comments。
- GitLab：`glab api projects/{id}/merge_requests/{iid}/notes?sort=asc&order_by=created_at`。
- Actor 缺失则丢弃（同 `ListIssueComments`）；增量游标必须在穷尽分页后推进。
- GitHub review comments（行内评论）位于另一端点，V0 不解析；不得一边合并该端点、一边声称忽略行内评论。

### 4.12 `GetChecks`
```
GetChecks(ctx, project, headSHA string) → (CheckSuite, error)
RerunCheck(ctx, project, checkRunID, expectedHeadSHA string) → error
```
**用途**：CI 状态与失败任务清单。

**归一要点**：
- GitHub：合并 Checks API (`/commits/{sha}/check-runs` / `check-suites`) 与 Statuses API (`/commits/{sha}/status`)。结论取最差（`failure` > `pending` > `success`）。Rerun 仅对带稳定 check-suite/run ID 且经 SHA 复验的对象调用对应 rerun endpoint；Status API-only failure 不可 rerun。
- GitLab：`glab api projects/{id}/pipelines?sha={sha}` 选最新 pipeline，再查询该 pipeline 的 jobs；从 jobs 归一失败项与 `allow_failure`。每个端点调用分别收费。Rerun 使用该 job 的 retry 端点，但在调用前/后均确认 job 的 pipeline SHA；无法作此确认时不调用。
- CI 结论归一为 `"success" | "failure" | "pending" | "unknown"`（两平台无法确定时用 `"unknown"`，不猜）。
- `RerunCheck` 只供 Gate 的一次 flaky rerun operation 使用。适配器必须先验证该 check/pipeline 仍属于 `expectedHeadSHA`，再调用平台 rerun API；不能验证、目标不唯一或平台 API 不支持该条件语义时分别返回 `ContractViolation`、`SemanticConflict` 或 `AuthOrCapability`。它不接受 pipeline 名称、URL 或任意请求参数作为替代身份。
- `failed_jobs` 列表：每项含 `name`、`web_url`（链接到具体 job/run 供人查看）、`allow_failure`（允许失败的 job 不计入整体失败）。

**`CheckSuite` 结构**：
```
Conclusion    string       // "success" | "failure" | "pending" | "unknown"
FailedJobs   []CheckJob   // 仅 Conclusion=failure 时填充
ExternalURL  string        // CI 详情页 URL（GitHub: check suite URL; GitLab: pipeline URL）
```

**`CheckJob` 结构**：
```
ID           string   // 非空、平台稳定的 check run/job ID；RerunCheck 的唯一目标
Name         string
WebURL       string
AllowFailure bool
```

### 4.13 `MergeChange`
```
MergeChange(ctx, project, changeID, expectedHeadSHA, method string) → (Change, error)
```
**用途**：**条件合并**——远端当前 head 必须仍等于 Gate 裁定的 `expectedHeadSHA`，否则拒绝并返回 `SemanticConflict`（DESIGN §6.4 / ADR-011）。

**归一要点**：
- V0 的 `method` **只能为 `"merge"`**，与 [`outbox.md` §8](outbox.md) 一致；其他值为 `ErrContractViolation`。平台差异表中的 squash/rebase/ff 不是可直接原样映射的共同枚举，后续开放方法必须版本化本契约并逐平台验证语义。
- GitHub：`gh api /repos/{org}/{repo}/pulls/{number}/merge -f sha='{expectedHeadSHA}' -f merge_method='merge'`。`sha` 参数提供原子 CAS。
- GitLab：`glab api projects/{id}/merge_requests/{iid}/merge -f sha='{expectedHeadSHA}'`。GitLab merge API 没有与 GitHub `merge_method` 同义的字段；项目的 fast-forward 策略由远端配置决定。`merge when pipeline succeeds` 不适用。
- **适配器无法提供 `sha` 参数的条件语义时**（如 CLI 无法提交 request body，或平台不支持 head CAS），`MergeChange` 返回 `ErrAuthOrCapability` 并标注 `capability_unsupported`。启动期未证明或持久化 capability 为不可用时均不得自动合并；运行期发现该错误也不得降级为无条件 merge（ADR-011 / DESIGN §8.1）。
- 远端返回非 200 且原因是 head SHA 不匹配 → `ErrSemanticConflict`。
- 合并成功后返回更新后的 `Change`（`State=ChangeMerged`、`MergedAt` 取自远端响应）。

## 5. 辅助类型

### `Comment`
```
ID        string
Author    string   // 必填；缺失时整条 Comment 在适配器内丢弃
Body      string
CreatedAt time.Time
```
用于 `ListIssueComments` / `ListChangeComments` 的返回值。

### `FindResult`
```
"marker_hit" | "no_match" | "semantic_conflict"
```
`FindChangeForCreateOperation` 的返回枚举，见 §4.8。

## 6. 平台差异清单与归一函数

以下差异必须在适配层内显式处理，每项对应一个归一函数。上层不可见 `number`/`iid`、`mergeable_state`、`Draft:` 前缀这类平台原语。

| 概念 | GitHub | GitLab | 归一处理 |
|------|--------|--------|---------|
| 变更编号 | `number` | `iid`（项目内）与全局 `id` 不同 | `changeID` 统一用 `number`/`iid`；全局 `id` 在适配层内仅用于 API 路径（GitLab 多数端点用 `iid`） |
| Issue 编号 | `number` | `iid`（同变更） | `issueID` 统一用 `number`/`iid` |
| 可合并性 | `mergeable` (bool) + `mergeable_state` | `merge_status` + `detailed_merge_status` | 归一到 `Change.Mergeability`；计算中或无法确定为 `unknown` |
| CI | Checks API + Statuses API（两套） | Pipelines + jobs | §4.12 合并数据源并取最差结论 |
| 审查通过 | Reviews (`APPROVED`) | Approvals（依赖版本/套餐） | 归一到 `Change.ReviewState`；无 Approvals 能力为 `unknown`，不得当作通过 |
| 草稿 | `draft` 布尔字段 | 标题 `Draft:` / `WIP:` 前缀 | 归一到 `Change.IsDraft`，GitLab 前缀匹配不区分大小写 |
| 合并方式 | merge / squash / rebase | 项目策略 merge / ff，另有 squash 开关 | V0 端口只接受 `merge`；不得把平台原语原样映射 |
| 增量拉取 | `since` + `sort=updated` | `updated_after` | 归一为适配器不透明 `Cursor` |
| 标签事件 actor | issue events / timeline | resource label events | 统一为 `LabelEvent.Actor`，缺即丢弃 |

**判定原则**：遇到语义差异一律取保守交集。凡是两个平台无法都给出确定性答案的问题，归一结果显式 `unknown`，由上层转 HITL——**适配器不猜**。

## 7. actor 契约

这是 C8 与 PRD §9.2 在适配层的结构保证（DESIGN §8.1「actor 是类型的一部分」）：

| 动词 | actor 来源 | 缺失行为 |
|------|-----------|---------|
| `ListLabelEvents` | `LabelEvent.Actor` | 丢弃整条驱动性事件 |
| `ListIssueComments` | `Comment.Author` | 丢弃该条评论 |
| `ListChangeComments` | `Comment.Author` | 丢弃该条评论 |

注意：`Issue.Author` 与 `Change`（无 author 字段）不在此列——`Author` 是 Issue 的创建者，属于事实观测而非驱动性事件，不对其施加 actor 闸门（PRD §4.5）。

**实现约束**：丢弃行为发生在适配器内的归一函数，调用方永远看不到空 actor 的对象。这比在每个调用点记得检查更强——是类型系统保证的 fail closed。

## 8. 进程调用边界

所有 `gh` / `glab` 调用必须：

1. **argv 数组**启动子进程（如 `exec.Command("gh", "api", "/repos/...")`），不得使用 `sh -c` 或任何 shell 字符串拼接。
2. **主机显式传递**：每次调用按 `ProjectRef.Host` 传 CLI hostname 参数；GitLab `ProjectKey` 进入 URL path 前必须做 path-segment 编码。不得把企业实例误发到默认公有主机。
3. **stdin** 传递 markdown/body；不得把用户正文放进可被进程列表读取的 argv。普通短标量可用 argv field 参数。
4. **分页**：列表动词逐页调用 CLI，每个子进程只发一个可计数的 HTTP 请求；穷尽分页前不得返回 `no_match` 或推进游标。禁止用一次 `--paginate` 子进程隐藏多个远端请求而少收费。
5. **超时**：每次调用带 context deadline，由上层注入。适配器不设自己的全局超时（不同动词的预期延迟不同）。
6. **stdout/stderr**：响应从 stdout 解析；stderr 截断、脱敏后进入 `Summary`，不得混流。需要 rate-limit header 时显式请求并在边界剥离 header。
7. **环境**：继承 `siftd` 环境与 CLI 登录态；适配器不得自行落盘 forge 凭证。

## 9. API 预算收费口

API 调用只在 Forge 适配层收费（DESIGN §9.2）。预算的唯一收费口是适配器内部计数器，上层不感知 `gh`/`glab` 的速率限制细节。

| 规则 | 说明 |
|------|------|
| 每个远端 HTTP 请求计一次 | V0 令一次 CLI 子进程只发一个请求，因此调用前收费一次；304 与失败请求也收费。多页与组合端点逐次收费，禁止 `--paginate` 隐藏请求数 |
| 幂等收费 | outbox 调用使用 [`outbox.md`](outbox.md) 的 `forge-call:<outbox_attempt_id>:<call_seq>`；Intake/观测调用使用同等稳定的 tick/project/call-seq 键，重放收费事务不得重复扣同一请求 |
| 预算状态持久化 | 写入 [`storage.md` §9.1](storage.md) 的 `budget_counters`，`kind=forge_api`、固定小时桶；重启恢复同一桶，不重新计数 |
| 接近上限 | 消耗 ≥ 80% 时把 Intake 降为配置的慢轮询并生成一次告警级通知 |
| 达到上限 | 收费 CAS 拒绝后不启动 CLI，返回 `ErrRateLimited`；非 Intake 动作也不得绕过收费口 |
| 远端 rate limit 联动 | 429/rate-limit 响应解析可信 `Retry-After`/reset 到 `ClassifiedError.RetryAt`；本地退避尊重该时间，但已发出的请求仍计费 |
| 窗口 reset | 使用 storage 定义的 UTC 固定小时桶；不得在本规格另造滑动窗口或 `api_quotas` 表 |

**收费点位置**：适配器内部统一的 `chargeAPICall(ctx, project, chargeKey)`，每次 CLI 子进程启动前先以稳定键收费。这是 DESIGN §9.2 规定的唯一收费口。

## 10. 契约测试与 fixture 录制

双平台跑同一套契约测试（DESIGN §8.1 / WBS V3）：

- 用真实 CLI 输出录成 fixture（`testdata/fixtures/github/`、`testdata/fixtures/gitlab/`）。
- 覆盖：分页、actor 缺失、限流、平台差异（`number` vs `iid`、Checks vs Pipelines、Draft 前缀）、Change marker 跨全状态唯一查找与同 base/head 冲突、merge 的远端 expected-head CAS。
- 录制成本近零——开发时本来就在敲这些命令。
- fixture 入 git（go:embed）。
- 每个边界类型的 golden test（DESIGN §5.2）：`closed` 契约断言额外字段 / 必填缺失被拒；Forge `open-envelope` 契约断言无关新增字段接受、必需字段缺失/变型被拒。

## 11. M1 fake 边界

M1 的 `Fake`（[`internal/forge/fake.go`](../../internal/forge/fake.go)）实现了以下骨架子集；其类型/签名不是 M2 已完成证据：

| 已实现 | 未实现（M2） |
|--------|-------------|
| `ListIssuesByLabel` | `GetIssue` |
| `ListLabelEvents`（待补 `TargetRef`/游标） | `ListIssueComments` |
| `GetChange`（待补完整投影） | `CommentTarget` |
| | `SetLabels` |
| | `CreateChange` |
| | `FindChangeForCreateOperation` |
| | `GetChangeDiff` |
| | `ListChangeComments` |
| | `GetChecks` |
| | `MergeChange` |

M2 的 `Fake` 需扩展以覆盖全部 13 个动词，且每个新动词遵循与真实适配器相同的契约（actor 必填、错误分类、marker 搜索、merge CAS 等）。fake 的 scripted 数据必须携带所有必填字段；缺失即 panic（当前已如此），确保测试不能通过残缺数据伪装边界。

M2 fake 不要求模拟 API 预算（收费口测试另用 mock），不要求模拟远端 rate limit 联动。

## 12. 交叉引用

| 来源 | 关联 |
|------|------|
| PRD §5.2 | 最小动词集、平台差异清单、自适应轮询参数 |
| PRD §9.2 | actor 鉴权：驱动性事件必须解析 actor |
| PRD §5.4 | merge 必须条件合并 |
| DESIGN §8.1 | Forge 适配层职责、归一在边界完成、argv/错误分类/契约测试 |
| DESIGN §6.4 | 逐类副作用幂等协议（marker 搜索、expected-head CAS） |
| DESIGN §9.2 | API 预算唯一收费口 |
| ADR-011 | merge expected-head CAS |
| [WBS M2 §2.1–§2.5](../WBS.md) | M2 任务分解与门禁 |
| [`brain.md`](brain.md) | T1/T3/T5 消费 Forge verb 的输入 |
| [`storage.md`](storage.md) | `budget_counters` 的 forge API 固定小时桶 |
| [`outbox.md`](outbox.md) | 评论/Change/merge worker 协议、Forge 收费键与 V0 merge method |
| [`control-plane.md`](control-plane.md) | 与 forge 无关（siftd 内部控制面） |

---

_字段级审定 | 2026-07-29 | active | [评审报告](../reviews/2026-07-29-forge-review-pi-gpt-5.6-sol.md)_
