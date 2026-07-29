// Package hooks captures the effective Git hooks state used for drift detection.
package hooks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

type Snapshot struct {
	GitConfigDigest    string
	CoreHooksPathValue *string
	EffectiveHooksPath string
	DirectoryDigest    string
	Digest             string
}

func Capture(ctx context.Context, repo string) (Snapshot, error) {
	configPath, err := gitPath(ctx, repo, "config")
	if err != nil {
		return Snapshot{}, err
	}
	if !filepath.IsAbs(configPath) {
		configPath = filepath.Join(repo, configPath)
	}
	configDigest, err := fileDigest(configPath)
	if err != nil {
		return Snapshot{}, fmt.Errorf("git config: %w", err)
	}
	raw, err := gitOutput(ctx, repo, "config", "--local", "--get", "core.hooksPath")
	if err != nil {
		if strings.TrimSpace(raw) != "" {
			return Snapshot{}, err
		}
		raw = ""
	}
	var value *string
	if strings.TrimSpace(raw) != "" {
		v := strings.TrimSpace(raw)
		value = &v
	}
	effective, err := gitPath(ctx, repo, "hooks")
	if err != nil {
		return Snapshot{}, err
	}
	if !filepath.IsAbs(effective) {
		effective = filepath.Join(filepath.Dir(configPath), effective)
	}
	effective, err = filepath.Abs(effective)
	if err != nil {
		return Snapshot{}, err
	}
	directoryDigest, err := directoryDigest(effective)
	if err != nil {
		return Snapshot{}, fmt.Errorf("hooks directory: %w", err)
	}
	h := sha256.New()
	fmt.Fprintf(h, "git-config\x00%s\x00", configDigest)
	if value != nil {
		fmt.Fprintf(h, "core-hooks-path\x00%s\x00", *value)
	} else {
		fmt.Fprint(h, "core-hooks-path\x00\x00")
	}
	fmt.Fprintf(h, "effective\x00%s\x00directory\x00%s", effective, directoryDigest)
	return Snapshot{configDigest, value, effective, directoryDigest, hex.EncodeToString(h.Sum(nil))}, nil
}

func gitPath(ctx context.Context, repo string, arg string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "rev-parse", "--git-path", arg)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}
func gitOutput(ctx context.Context, repo string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}
func fileDigest(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

func directoryDigest(root string) (string, error) {
	type entry struct {
		name string
		mode os.FileMode
		data []byte
	}
	var entries []entry
	info, err := os.Stat(root)
	if os.IsNotExist(err) {
		info = nil
	} else if err != nil {
		return "", err
	}
	if info != nil && !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", root)
	}
	if info != nil {
		err = filepath.Walk(root, func(path string, i os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if path == root {
				return nil
			}
			rel, e := filepath.Rel(root, path)
			if e != nil {
				return e
			}
			data, e := os.ReadFile(path)
			if i.IsDir() {
				data = nil
			} else if e != nil {
				return e
			}
			entries = append(entries, entry{filepath.ToSlash(rel), i.Mode(), data})
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	h := sha256.New()
	for _, e := range entries {
		fmt.Fprintf(h, "%s\x00%o\x00", e.name, e.mode)
		h.Write(e.data)
		h.Write([]byte{0})
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum), nil
}
