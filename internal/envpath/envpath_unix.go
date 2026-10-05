//go:build !windows

// Package envpath discovers user-local tool directories that belong on PATH for
// an interactive/agent shell but are typically absent from a systemd service's
// minimal PATH. It mirrors the intent of buildEnvBlock() on Windows: a PTY shell
// launched by the service must find CLI agents (claude, codex, gemini, …) that
// live under the user profile (~/.local/bin, npm-global, nvm, …).
package envpath

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// UserBinDirs returns existing user-local tool directories, most-preferred
// first, deduped. Only directories that actually exist are returned so we never
// pollute PATH with dead entries. `home` may be empty — it is resolved from
// $HOME / os.UserHomeDir() in that case.
func UserBinDirs(home string) []string {
	if home == "" {
		home = os.Getenv("HOME")
	}
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = h
		}
	}

	var candidates []string
	if home != "" {
		candidates = append(candidates,
			filepath.Join(home, ".local", "bin"),
			filepath.Join(home, "bin"),
			filepath.Join(home, ".npm-global", "bin"),
			filepath.Join(home, ".npm-packages", "bin"),
			filepath.Join(home, ".node_modules", "bin"),
			filepath.Join(home, ".yarn", "bin"),
			filepath.Join(home, ".config", "yarn", "global", "node_modules", ".bin"),
			filepath.Join(home, ".bun", "bin"),
			filepath.Join(home, ".deno", "bin"),
			filepath.Join(home, ".cargo", "bin"),
			filepath.Join(home, "go", "bin"),
		)
		// nvm: prefer the default-aliased node, then the newest installed.
		candidates = append(candidates, nvmBinDirs(home)...)
	}
	// System locations a minimal service PATH may still miss.
	candidates = append(candidates,
		"/usr/local/bin",
		"/snap/bin",
		"/opt/homebrew/bin", // macOS arm64
	)

	return existingDirs(candidates)
}

// existingDirs filters to directories that exist, deduping while preserving
// order.
func existingDirs(candidates []string) []string {
	seen := make(map[string]bool, len(candidates))
	out := make([]string, 0, len(candidates))
	for _, d := range candidates {
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		if info, err := os.Stat(d); err == nil && info.IsDir() {
			out = append(out, d)
		}
	}
	return out
}

// nvmBinDirs returns the bin dirs of the default-aliased and newest nvm-managed
// node versions (existence filtered later by existingDirs).
func nvmBinDirs(home string) []string {
	root := filepath.Join(home, ".nvm", "versions", "node")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var versions []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "v") {
			versions = append(versions, e.Name())
		}
	}
	if len(versions) == 0 {
		return nil
	}

	var out []string
	// Default alias (~/.nvm/alias/default may hold a concrete version or a name).
	if raw, err := os.ReadFile(filepath.Join(home, ".nvm", "alias", "default")); err == nil {
		want := strings.TrimSpace(string(raw))
		if !strings.HasPrefix(want, "v") {
			want = "v" + want
		}
		for _, v := range versions {
			if v == want {
				out = append(out, filepath.Join(root, v, "bin"))
			}
		}
	}
	// Newest installed by numeric semver.
	newest := versions[0]
	for _, v := range versions[1:] {
		if compareNodeVersion(v, newest) > 0 {
			newest = v
		}
	}
	out = append(out, filepath.Join(root, newest, "bin"))
	return out
}

// compareNodeVersion compares "vMAJOR.MINOR.PATCH" strings numerically.
func compareNodeVersion(a, b string) int {
	pa, pb := parseNodeVersion(a), parseNodeVersion(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] > pb[i] {
				return 1
			}
			return -1
		}
	}
	return 0
}

func parseNodeVersion(v string) [3]int {
	v = strings.TrimPrefix(v, "v")
	parts := strings.SplitN(v, ".", 3)
	var out [3]int
	for i := 0; i < 3 && i < len(parts); i++ {
		out[i], _ = strconv.Atoi(parts[i])
	}
	return out
}
