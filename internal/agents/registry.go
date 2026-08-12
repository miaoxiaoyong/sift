// Package agents holds built-in system knowledge about known coding agents:
// their default characteristic profiles (strengths, context window, cost
// tier, speed, one-line notes) and version probing. This data ships with sift
// and updates per release; it is deliberately NOT part of config.yaml (config
// stays minimal: id/executable/args). Future brain routing will look up the
// registry by agent id (issue #930).
package agents

import (
	"context"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Cost is a coarse pricing tier shown as a routing hint.
type Cost string

const (
	CostFree   Cost = "free"
	CostLow    Cost = "low"
	CostMedium Cost = "medium"
	CostHigh   Cost = "high"
)

// Speed is a coarse response speed tier shown as a routing hint.
type Speed string

const (
	SpeedFast   Speed = "fast"
	SpeedMedium Speed = "medium"
	SpeedSlow   Speed = "slow"
)

// Characteristic is the built-in profile of a known coding agent. Strengths
// uses canonical tag ids (coding/reasoning/review/planning/long-context/
// ide-native); Chinese display labels live in StrengthLabel.
type Characteristic struct {
	Strengths []string
	Context   string // context window magnitude ("200K"); "—" when unknown
	Cost      Cost
	Speed     Speed
	Notes     string // one-line Chinese note shown in the wizard
}

// registry maps executable base names to built-in profiles. It is keyed by
// the executable name (config stores id/executable only; routing looks up by
// agent id, issue #930).
var registry = map[string]Characteristic{
	"claude": {
		Strengths: []string{"coding", "reasoning", "long-context"},
		Context:   "200K",
		Cost:      CostMedium,
		Speed:     SpeedMedium,
		Notes:     "Anthropic Claude Code：长上下文推理与多文件改造强",
	},
	"codex": {
		Strengths: []string{"coding", "review"},
		Context:   "200K",
		Cost:      CostMedium,
		Speed:     SpeedFast,
		Notes:     "OpenAI Codex CLI：执行快，适合批量机械性修改",
	},
	"cursor": {
		Strengths: []string{"ide-native", "coding", "planning"},
		Context:   "200K",
		Cost:      CostHigh,
		Speed:     SpeedMedium,
		Notes:     "Cursor CLI：编辑器原生体验，适合边看边改",
	},
	"pi": {
		Strengths: []string{"coding", "planning", "review"},
		Context:   "200K",
		Cost:      CostHigh,
		Speed:     SpeedMedium,
		Notes:     "pi 编码代理（可换多模型）：适合编排与深度修改",
	},
	"gemini": {
		Strengths: []string{"coding", "reasoning", "long-context"},
		Context:   "1M",
		Cost:      CostLow,
		Speed:     SpeedMedium,
		Notes:     "Google Gemini CLI：超长上下文，适合大仓库全局分析",
	},
	"gemini-cli": {
		Strengths: []string{"coding", "reasoning", "long-context"},
		Context:   "1M",
		Cost:      CostLow,
		Speed:     SpeedMedium,
		Notes:     "Google Gemini CLI：超长上下文，适合大仓库全局分析",
	},
	"aider": {
		Strengths: []string{"coding", "planning"},
		Context:   "200K",
		Cost:      CostLow,
		Speed:     SpeedFast,
		Notes:     "开源终端结对编程：git 原生集成，成本低",
	},
	"qwen": {
		Strengths: []string{"coding", "long-context"},
		Context:   "128K",
		Cost:      CostLow,
		Speed:     SpeedMedium,
		Notes:     "阿里 Qwen Code：中文友好，性价比高",
	},
	"cody": {
		Strengths: []string{"ide-native", "coding"},
		Context:   "200K",
		Cost:      CostMedium,
		Speed:     SpeedMedium,
		Notes:     "Sourcegraph Cody：IDE 原生助手",
	},
}

// generic is the fallback profile for coding agents the registry does not
// know yet; users can still add them by executable name.
var generic = Characteristic{
	Strengths: []string{"coding"},
	Context:   "—",
	Cost:      CostMedium,
	Speed:     SpeedMedium,
	Notes:     "未收录的编码工具，使用通用配置",
}

// Known returns the ordered executable base names the wizard auto-detects.
// Every name must have a profile in registry (guarded by TestKnownRegistered).
func Known() []string {
	return []string{"claude", "codex", "cursor", "pi", "gemini", "gemini-cli", "aider", "qwen", "cody"}
}

// For returns the built-in profile of an executable (base-name lookup), or
// the generic profile for unknown agents.
func For(executable string) Characteristic {
	if c, ok := registry[filepath.Base(executable)]; ok {
		return c
	}
	return generic
}

// StrengthLabel maps canonical strength tag ids to Chinese display labels.
var StrengthLabel = map[string]string{
	"coding":       "编码",
	"reasoning":    "推理",
	"review":       "审查",
	"planning":     "规划",
	"long-context": "长上下文",
	"ide-native":   "IDE 原生",
}

// CostLabel maps cost tiers to Chinese display labels.
var CostLabel = map[Cost]string{
	CostFree: "免费", CostLow: "低", CostMedium: "中", CostHigh: "高",
}

// SpeedLabel maps speed tiers to Chinese display labels.
var SpeedLabel = map[Speed]string{
	SpeedFast: "快", SpeedMedium: "中", SpeedSlow: "慢",
}

// Summary renders the characteristic line fragment used by the wizard:
// "编码·推理·长上下文 · 200K · 中 · 中".
func (c Characteristic) Summary() string {
	var tags []string
	for _, t := range c.Strengths {
		if label, ok := StrengthLabel[t]; ok {
			tags = append(tags, label)
		} else {
			tags = append(tags, t)
		}
	}
	cost, speed := CostLabel[c.Cost], SpeedLabel[c.Speed]
	if cost == "" {
		cost = string(c.Cost)
	}
	if speed == "" {
		speed = string(c.Speed)
	}
	return strings.Join(tags, "·") + " · " + c.Context + " · " + cost + " · " + speed
}

// ProbeVersion runs "<exe> --version" under a short timeout and returns the
// version token of the first output line, or "" when the binary has no
// version flag, errors, or times out. It is display-only knowledge: the
// version never enters config.yaml (issue #930).
func ProbeVersion(exe string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, exe, "--version").CombinedOutput()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		// "Claude Code version 2.0.0" → the token after the keyword.
		if i := strings.Index(strings.ToLower(line), "version"); i >= 0 {
			if rest := strings.TrimSpace(line[i+len("version"):]); rest != "" {
				return versionToken(rest)
			}
		}
		return versionToken(line)
	}
	return ""
}

// versionToken extracts the first semver-ish token from s (e.g. "codex-cli
// 0.145.0" → "0.145.0"); when none exists it returns s itself. Pure integers
// are not treated as versions.
var versionTokenRe = regexp.MustCompile(`v?\d+(?:\.\d+){1,3}[0-9A-Za-z.\-+]*`)

func versionToken(s string) string {
	if m := versionTokenRe.FindString(s); m != "" {
		return m
	}
	return s
}
