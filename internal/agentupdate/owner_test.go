package agentupdate

import (
	"strings"
	"testing"
)

func winEnv() Env {
	return Env{
		GOOS:         "windows",
		Home:         `C:\Users\user`,
		AppData:      `C:\Users\user\AppData\Roaming`,
		LocalAppData: `C:\Users\user\AppData\Local`,
		NPMPrefix:    `C:\Users\user\AppData\Roaming\npm`,
		Exists: func(p string) bool {
			// Так выглядит машина владельца 29.09.2026: npm-пакеты в %APPDATA%\npm.
			p = strings.ToLower(normPath(p))
			return strings.HasPrefix(p, "c:/users/user/appdata/roaming/npm/node_modules/")
		},
	}
}

func posixEnv(links map[string]string) Env {
	return Env{
		GOOS:      "linux",
		Home:      "/home/me",
		NPMPrefix: "/usr/local",
		Resolve:   func(p string) string { return links[p] },
	}
}

func TestDetectWindows(t *testing.T) {
	cases := []struct {
		name, id, path, pkg string
		owner, want         string // want — Name/Prefix/Package по смыслу
	}{
		{"npm claude (машина владельца)", "claude", `C:\Users\user\AppData\Roaming\npm\claude.cmd`, "@anthropic-ai/claude-code", OwnerNPM, `C:/Users/user/AppData/Roaming/npm`},
		{"npm codex", "codex", `C:\Users\user\AppData\Roaming\npm\codex.cmd`, "@openai/codex", OwnerNPM, `C:/Users/user/AppData/Roaming/npm`},
		{"нативный claude", "claude", `C:\Users\user\.local\bin\claude.exe`, "@anthropic-ai/claude-code", OwnerClaudeNative, ""},
		{"регистр пути не важен", "claude", `c:\users\USER\.local\bin\claude.exe`, "@anthropic-ai/claude-code", OwnerClaudeNative, ""},
		{"pnpm", "codex", `C:\Users\user\AppData\Local\pnpm\codex.cmd`, "@openai/codex", OwnerPNPM, ""},
		{"bun", "gemini", `C:\Users\user\.bun\bin\gemini.exe`, "@google/gemini-cli", OwnerBun, ""},
		{"volta", "codex", `C:\Users\user\AppData\Local\Volta\bin\codex.exe`, "@openai/codex", OwnerVolta, ""},
		{"scoop shim", "aider", `C:\Users\user\scoop\shims\aider.exe`, "", OwnerScoop, "aider"},
		{"winget", "amazon-q", `C:\Users\user\AppData\Local\Microsoft\WinGet\Packages\Amazon.AmazonQ_Microsoft.Winget.Source_8wekyb3d8bbwe\q.exe`, "", OwnerWinget, "Amazon.AmazonQ"},
		{"неизвестно", "aider", `C:\Python313\Scripts\aider.exe`, "", OwnerUnknown, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := Detect(tc.id, tc.path, tc.pkg, winEnv())
			if in.Owner != tc.owner {
				t.Fatalf("owner = %q, want %q", in.Owner, tc.owner)
			}
			got := in.Name
			if tc.owner == OwnerNPM {
				got = in.Prefix
			}
			if got != tc.want {
				t.Fatalf("name/prefix = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDetectPOSIX(t *testing.T) {
	links := map[string]string{
		"/usr/local/bin/codex":                           "/usr/local/lib/node_modules/@openai/codex/bin/codex.js",
		"/home/me/.nvm/versions/node/v22.1.0/bin/claude": "/home/me/.nvm/versions/node/v22.1.0/lib/node_modules/@anthropic-ai/claude-code/cli.js",
		"/home/me/.local/bin/claude":                     "/home/me/.local/share/claude/versions/2.1.284",
		"/opt/homebrew/bin/codex":                        "/opt/homebrew/Caskroom/codex/0.158.0/codex",
		"/opt/homebrew/bin/gemini":                       "/opt/homebrew/Cellar/gemini-cli/0.61.0/bin/gemini",
		"/home/me/.local/share/pnpm/kimi":                "/home/me/.local/share/pnpm/global/5/node_modules/@moonshot-ai/kimi-code/dist/main.mjs",
	}
	env := posixEnv(links)
	cases := []struct {
		id, path, pkg, owner string
	}{
		{"codex", "/usr/local/bin/codex", "@openai/codex", OwnerNPM},
		{"claude", "/home/me/.nvm/versions/node/v22.1.0/bin/claude", "@anthropic-ai/claude-code", OwnerNPM},
		{"claude", "/home/me/.local/bin/claude", "@anthropic-ai/claude-code", OwnerClaudeNative},
		{"claude", "/home/me/.claude/local/claude", "@anthropic-ai/claude-code", OwnerClaudeNative},
		{"codex", "/opt/homebrew/bin/codex", "@openai/codex", OwnerBrew},
		{"gemini", "/opt/homebrew/bin/gemini", "@google/gemini-cli", OwnerBrew},
		{"kimi", "/home/me/.local/share/pnpm/kimi", "@moonshot-ai/kimi-code", OwnerPNPM},
		{"codex", "/home/me/.bun/bin/codex", "@openai/codex", OwnerBun},
		{"codex", "/home/me/.volta/bin/codex", "@openai/codex", OwnerVolta},
		// ~/.local/bin у НЕ-claude — это pipx/установщики, не нативный Claude.
		{"cursor-agent", "/home/me/.local/bin/cursor-agent", "", OwnerUnknown},
		{"aider", "/usr/bin/aider", "", OwnerUnknown},
	}
	for _, tc := range cases {
		in := Detect(tc.id, tc.path, tc.pkg, env)
		if in.Owner != tc.owner {
			t.Errorf("%s: owner = %q, want %q", tc.path, in.Owner, tc.owner)
		}
	}
	// nvm: префикс берём из пути, а не из `npm prefix -g`.
	in := Detect("claude", "/home/me/.nvm/versions/node/v22.1.0/bin/claude", "@anthropic-ai/claude-code", env)
	if in.Prefix != "/home/me/.nvm/versions/node/v22.1.0" || in.Package != "@anthropic-ai/claude-code" {
		t.Fatalf("nvm: prefix=%q pkg=%q", in.Prefix, in.Package)
	}
	if b := Detect("codex", "/opt/homebrew/bin/codex", "", env); !b.Cask || b.Name != "codex" {
		t.Fatalf("cask: %+v", b)
	}
	if b := Detect("gemini", "/opt/homebrew/bin/gemini", "", env); b.Cask || b.Name != "gemini-cli" {
		t.Fatalf("formula: %+v", b)
	}
}

// Регистр в Linux различается: /Home/Me — не домашний каталог /home/me.
func TestDetectPOSIXCaseSensitive(t *testing.T) {
	in := Detect("claude", "/Home/Me/.local/bin/claude", "", posixEnv(nil))
	if in.Owner == OwnerClaudeNative {
		t.Fatal("на Linux регистр пути значим")
	}
}

func TestPackageFromInstall(t *testing.T) {
	cases := map[string]string{
		"npm i -g @anthropic-ai/claude-code":    "@anthropic-ai/claude-code",
		"npm.cmd i -g @openai/codex":            "@openai/codex",
		"npm install --global opencode-ai":      "opencode-ai",
		"npm i -g kilocode@latest":              "kilocode",
		"npm i -g @scope/name@1.2.3":            "@scope/name",
		"pip install aider-chat":                "",
		"winget install -e --id Amazon.AmazonQ": "",
		"npm i @openai/codex":                   "", // не глобально
		"":                                      "",
	}
	for in, want := range cases {
		if got := PackageFromInstall(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}
