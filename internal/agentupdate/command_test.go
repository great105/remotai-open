package agentupdate

import "testing"

func TestBuildCommand(t *testing.T) {
	win := winEnv()
	posix := posixEnv(nil)
	look := func(names ...string) LookPath {
		return func(n string) bool {
			for _, x := range names {
				if x == n {
					return true
				}
			}
			return false
		}
	}
	cases := []struct {
		name string
		in   Install
		env  Env
		look LookPath
		want string
		can  bool
	}{
		{"npm windows → npm.cmd (не .ps1)", Install{Owner: OwnerNPM, Package: "@openai/codex", Prefix: `C:/Users/user/AppData/Roaming/npm`}, win, nil, "npm.cmd i -g @openai/codex@latest", true},
		{"npm posix", Install{Owner: OwnerNPM, Package: "@openai/codex", Prefix: "/usr/local"}, posix, nil, "npm i -g @openai/codex@latest", true},
		{"npm в другом префиксе (nvm) → --prefix туда же", Install{Owner: OwnerNPM, Package: "@anthropic-ai/claude-code", Prefix: "/home/me/.nvm/versions/node/v22.1.0"}, posix, nil,
			`npm i -g @anthropic-ai/claude-code@latest --prefix "/home/me/.nvm/versions/node/v22.1.0"`, true},
		{"npm без пакета", Install{Owner: OwnerNPM}, posix, nil, "", false},
		{"pnpm windows .exe", Install{Owner: OwnerPNPM, Package: "@openai/codex"}, win, look("pnpm.exe"), "pnpm.exe add -g @openai/codex@latest", true},
		{"pnpm windows .cmd", Install{Owner: OwnerPNPM, Package: "@openai/codex"}, win, look("pnpm.cmd"), "pnpm.cmd add -g @openai/codex@latest", true},
		{"pnpm posix", Install{Owner: OwnerPNPM, Package: "@openai/codex"}, posix, nil, "pnpm add -g @openai/codex@latest", true},
		{"bun", Install{Owner: OwnerBun, Package: "@google/gemini-cli"}, posix, nil, "bun add -g @google/gemini-cli@latest", true},
		{"volta", Install{Owner: OwnerVolta, Package: "@openai/codex"}, posix, nil, "volta install @openai/codex@latest", true},
		{"нативный claude posix", Install{Owner: OwnerClaudeNative, Bin: "claude"}, posix, nil, "claude update", true},
		{"нативный claude windows", Install{Owner: OwnerClaudeNative, Bin: "claude.exe"}, win, nil, "claude.exe update", true},
		{"brew formula", Install{Owner: OwnerBrew, Name: "gemini-cli"}, posix, nil, "brew upgrade gemini-cli", true},
		{"brew cask", Install{Owner: OwnerBrew, Name: "codex", Cask: true}, posix, nil, "brew upgrade --cask codex", true},
		{"brew без имени", Install{Owner: OwnerBrew}, posix, nil, "", false},
		{"winget", Install{Owner: OwnerWinget, Name: "Amazon.AmazonQ"}, win, nil, "winget upgrade -e --id Amazon.AmazonQ", true},
		{"scoop", Install{Owner: OwnerScoop, Name: "aider"}, win, nil, "scoop update aider", true},
		{"неизвестно", Install{Owner: OwnerUnknown}, win, nil, "", false},
		{"опасное имя пакета не подставляем", Install{Owner: OwnerNPM, Package: "x;rm -rf /"}, posix, nil, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := BuildCommand(tc.in, tc.env, tc.look)
			if c.Text != tc.want || c.CanUpdate != tc.can {
				t.Fatalf("got %q can=%v, want %q can=%v", c.Text, c.CanUpdate, tc.want, tc.can)
			}
			if !c.CanUpdate && c.Reason == "" {
				t.Fatal("отказ без причины для человека")
			}
		})
	}
	// Нативный Claude: последнюю версию без обновления не узнать.
	if BuildCommand(Install{Owner: OwnerClaudeNative}, posix, nil).LatestKnown {
		t.Fatal("у нативного установщика latest проверяет он сам")
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"2.1.3", "2.1.5", -1},
		{"2.1.10", "2.1.9", 1},
		{"0.37.2", "2.1.1", -1},
		{"1.0.4", "1.0.41", -1},
		{"1.2", "1.2.0", 0},
		{"1.2.0-beta.1", "1.2.0", -1},
		{"v1.2.0", "1.2.0", 0},
		{"", "1.0.0", -1},
		{"1.0.0", "", 1},
	}
	for _, tc := range cases {
		if got := CompareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("%q vs %q = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestParseVersion(t *testing.T) {
	// Живые ответы машины владельца 29.09.2026.
	cases := map[string]string{
		"2.1.284 (Claude Code)":   "2.1.284",
		"codex-cli 0.158.0":       "0.158.0",
		"grok 1.0.4 (d846eb93d9)": "1.0.4",
		"0.61.0\r\n":              "0.61.0",
		"kimi 0.37.2":             "0.37.2",
		"no version here":         "",
	}
	for in, want := range cases {
		if got := ParseVersion(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}
