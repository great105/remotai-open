package agentupdate

import (
	"regexp"
	"strconv"
	"strings"
)

// Живые ответы `--version` на машине владельца 29.09.2026 — все разные:
//
//	claude  → «2.1.284 (Claude Code)»
//	codex   → «codex-cli 0.158.0»
//	grok    → «grok 1.0.4 (d846eb93d9)»
//	gemini  → «0.61.0»
//
// Поэтому берём первое число вида X.Y[.Z][-pre], а не строку целиком.
var versionRe = regexp.MustCompile(`\d+\.\d+(?:\.\d+)?(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?`)

// ParseVersion достаёт номер версии из вывода CLI ("" — не нашли).
func ParseVersion(out string) string {
	return versionRe.FindString(out)
}

// CompareVersions: -1 если a<b, 0 если равны, 1 если a>b. Пустая версия
// меньше любой. Предрелиз (1.2.0-beta) меньше релиза (1.2.0).
func CompareVersions(a, b string) int {
	a, b = strings.TrimPrefix(strings.TrimSpace(a), "v"), strings.TrimPrefix(strings.TrimSpace(b), "v")
	if a == b {
		return 0
	}
	if a == "" {
		return -1
	}
	if b == "" {
		return 1
	}
	an, apre, _ := strings.Cut(a, "-")
	bn, bpre, _ := strings.Cut(b, "-")
	ap, bp := strings.Split(an, "."), strings.Split(bn, ".")
	for i := 0; i < len(ap) || i < len(bp); i++ {
		var x, y int
		if i < len(ap) {
			x, _ = strconv.Atoi(ap[i])
		}
		if i < len(bp) {
			y, _ = strconv.Atoi(bp[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	switch {
	case apre == bpre:
		return 0
	case apre == "":
		return 1
	case bpre == "":
		return -1
	case apre < bpre:
		return -1
	}
	return 1
}
