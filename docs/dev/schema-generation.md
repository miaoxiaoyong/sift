---
status: active
created: 2026-07-30
summary: JSON Schema 再生与漂移检查
---

# JSON Schema 再生

生成器位于独立嵌套模块 `tools/schema`，不属于根模块的 `go list ./...`、build 或 test package 集。运行时类型与已提交产物仍在根模块：

- Decode/边界类型：`internal/schema`
- Decode/配置 artifacts：`internal/schema/artifacts/*.schema.json`
- Brain artifacts：`internal/brain/prompts/T1..T7/v1.schema.json`

## 标准入口

在仓库根目录执行：

```bash
go generate ./...
```

根模块的两个 `//go:generate` 指令会通过 `go -C tools/schema` 显式调用工具模块，同时再生 Decode/配置和 Brain artifacts。

只再生一组：

```bash
go generate ./internal/schema
go generate ./internal/brain
```

直接调用工具模块：

```bash
go -C tools/schema run ./cmd/contracts -out ../../internal/schema/artifacts
go -C tools/schema run ./cmd/brain -out ../../internal/brain/prompts
```

## 提交前检查

```bash
git diff --check
git diff -- internal/schema/artifacts internal/brain/prompts
go test ./...
```

`.github/workflows/build.yml` 的 `schema-drift` job 会重新执行 `go generate ./...`，并在已提交 artifacts 与结构体定义不一致时失败。不得手写修改 `.schema.json` 后跳过再生。
