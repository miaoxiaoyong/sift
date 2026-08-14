package config

import (
	"fmt"
	"sort"
	"strings"
)

func dedupAllowlist(field string, in []string) ([]string, error) {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if strings.TrimSpace(s) == "" {
			return nil, configError(field, "contains empty login")
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
}

func normalizeAgents(raw []RawAgent, defaultConc int, defaultBackend Backend) ([]Agent, error) {
	out := make([]Agent, 0, len(raw))
	ids := make(map[string]int)
	for i, a := range raw {
		f := fmt.Sprintf("agents[%d]", i)
		if a.ID == nil || *a.ID == "" {
			return nil, configError(f+".id", "required")
		}
		if !agentIDRe.MatchString(*a.ID) {
			return nil, configError(f+".id", "must match ^[a-z][a-z0-9-]{0,62}$")
		}
		if prev, dup := ids[*a.ID]; dup {
			return nil, configError(f+".id", "duplicate id %q (first at agents[%d])", *a.ID, prev)
		}
		ids[*a.ID] = i
		if a.Executable == nil || *a.Executable == "" {
			return nil, configError(f+".executable", "required and must be non-empty")
		}

		transport := TaskTransportStdin
		if a.TaskTransport != nil {
			transport = *a.TaskTransport
		}
		backend := defaultBackend
		if a.Backend != nil {
			backend = *a.Backend
		}
		if err := validateArgs(f+".args", a.Args, transport); err != nil {
			return nil, err
		}

		maxConc := defaultConc
		if a.MaxConcurrent != nil {
			maxConc = *a.MaxConcurrent
			if err := checkIntRange(f+".max_concurrent", maxConc, 1, 32); err != nil {
				return nil, err
			}
		}

		versionArgs := []string{"--version"}
		if a.VersionArgs != nil {
			versionArgs = append([]string(nil), a.VersionArgs...)
		}

		launchEnv, err := normalizeLaunchEnv(f+".launch_env", a.LaunchEnv)
		if err != nil {
			return nil, err
		}

		out = append(out, Agent{
			ID:            *a.ID,
			Executable:    *a.Executable,
			Args:          append([]string(nil), a.Args...),
			TaskTransport: transport,
			Backend:       backend,
			MaxConcurrent: maxConc,
			VersionArgs:   versionArgs,
			LaunchEnv:     launchEnv,
		})
	}
	return out, nil
}

// launchEnvWhitelist is the closed key set of agents[].launch_env
// (config.md §3.2): HOME and PATH are not credentials, so the init-frozen
// snapshot may enter the qualification probe and production launch
// environment. No other key is accepted.
var launchEnvWhitelist = map[string]bool{"HOME": true, "PATH": true}

// normalizeLaunchEnv validates the frozen launch environment: whitelisted
// keys only, non-empty values without NUL bytes. The map is copied so the
// resolved config owns its snapshot.
func normalizeLaunchEnv(field string, in map[string]string) (map[string]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if !launchEnvWhitelist[k] {
			return nil, configError(field, "key %q is not in the HOME/PATH whitelist", k)
		}
		if v == "" {
			return nil, configError(field+"."+k, "must be non-empty")
		}
		if strings.ContainsRune(v, '\x00') {
			return nil, configError(field+"."+k, "contains NUL byte")
		}
		out[k] = v
	}
	return out, nil
}

// validateArgs enforces the argv contract (config.md §3.2): no NUL bytes, the
// only permitted placeholder is the whole token {task_file}, stdin forbids it
// and file requires exactly one.
func validateArgs(field string, args []string, transport TaskTransport) error {
	placeholders := 0
	for i, a := range args {
		if strings.ContainsRune(a, '\x00') {
			return configError(fmt.Sprintf("%s[%d]", field, i), "contains NUL byte")
		}
		if strings.Contains(a, taskFileToken) {
			if a != taskFileToken {
				return configError(fmt.Sprintf("%s[%d]", field, i), "{task_file} must be a whole argv token, not a substring")
			}
			placeholders++
		}
	}
	switch transport {
	case TaskTransportStdin:
		if placeholders != 0 {
			return configError(field, "task_transport=stdin forbids {task_file}")
		}
	case TaskTransportFile:
		if placeholders != 1 {
			return configError(field, "task_transport=file requires exactly one {task_file} argv token")
		}
	}
	return nil
}

func normalizeProjects(raw []RawProject, agents []Agent) ([]Project, error) {
	agentIDs := make(map[string]bool, len(agents))
	for _, a := range agents {
		agentIDs[a.ID] = true
	}

	out := make([]Project, 0, len(raw))
	ids := make(map[string]int)
	repoSeen := make(map[string]string) // cleaned repo -> project id
	for i, p := range raw {
		f := fmt.Sprintf("projects[%d]", i)
		if p.ID == nil || *p.ID == "" {
			return nil, configError(f+".id", "required")
		}
		if !agentIDRe.MatchString(*p.ID) {
			return nil, configError(f+".id", "must match ^[a-z][a-z0-9-]{0,62}$")
		}
		if prev, dup := ids[*p.ID]; dup {
			return nil, configError(f+".id", "duplicate id %q (first at projects[%d])", *p.ID, prev)
		}
		ids[*p.ID] = i
		if p.Repo == nil || *p.Repo == "" {
			return nil, configError(f+".repo", "required")
		}
		if !isAbsPath(*p.Repo) {
			return nil, configError(f+".repo", "must be an absolute path")
		}
		repo := cleanPath(*p.Repo)
		if p.Forge == nil {
			return nil, configError(f+".forge", "required")
		}
		if p.Forge.Kind == nil {
			return nil, configError(f+".forge.kind", "required")
		}
		if p.Forge.Project == nil || *p.Forge.Project == "" {
			return nil, configError(f+".forge.project", "required")
		}

		enabled := true
		if p.Enabled != nil {
			enabled = *p.Enabled
		}
		if enabled {
			if other, dup := repoSeen[repo]; dup {
				return nil, configError(f+".repo", "duplicate enabled repo %q (first used by project %q)", repo, other)
			}
			repoSeen[repo] = *p.ID
		}

		// Resolve per-project agent candidates. Empty means all defined agents.
		candidates := make([]string, 0, len(agents))
		if len(p.Agents) == 0 {
			for _, a := range agents {
				candidates = append(candidates, a.ID)
			}
		} else {
			seen := make(map[string]bool)
			for j, ref := range p.Agents {
				if !agentIDs[ref] {
					return nil, configError(fmt.Sprintf("%s.agents[%d]", f, j), "references unknown agent %q", ref)
				}
				if seen[ref] {
					continue
				}
				seen[ref] = true
				candidates = append(candidates, ref)
			}
		}

		host := p.Forge.Kind.defaultHost()
		if p.Forge.Host != nil && *p.Forge.Host != "" {
			host = *p.Forge.Host
		}
		cli := p.Forge.Kind.defaultCLI()
		if p.Forge.CLI != nil && *p.Forge.CLI != "" {
			cli = *p.Forge.CLI
		}

		out = append(out, Project{
			ID:      *p.ID,
			Repo:    repo,
			Enabled: enabled,
			Agents:  candidates,
			Forge: ForgeRef{
				Kind:    *p.Forge.Kind,
				Project: *p.Forge.Project,
				Host:    host,
				CLI:     cli,
			},
		})
	}
	return out, nil
}
