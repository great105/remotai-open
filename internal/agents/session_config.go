package agents

import "tgcontrol/internal/sessions"

// SessionConfigWithPermissionMode applies the generic ACP-style session
// permission mode to the concrete agent-specific config keys used at runtime.
func SessionConfigWithPermissionMode(agentType string, base map[string]string, permissionMode string) map[string]string {
	if permissionMode == "" {
		return base
	}

	merged := make(map[string]string, len(base)+1)
	for k, v := range base {
		merged[k] = v
	}

	switch agentType {
	case "claude":
		switch permissionMode {
		case sessions.PermApproveAll:
			merged["claude_permission_mode"] = "bypassPermissions"
		case sessions.PermApproveReads:
			merged["claude_permission_mode"] = "default"
		case sessions.PermDenyAll:
			merged["claude_permission_mode"] = "plan"
		default:
			merged["claude_permission_mode"] = permissionMode
		}
	case "codex":
		switch permissionMode {
		case sessions.PermApproveAll:
			merged["codex_approval_mode"] = "full-auto"
		case sessions.PermApproveReads:
			merged["codex_approval_mode"] = "suggest"
		case sessions.PermDenyAll:
			// Codex CLI has no plan-only mode, so use the most restrictive
			// interactive approval mode available.
			merged["codex_approval_mode"] = "suggest"
		default:
			merged["codex_approval_mode"] = permissionMode
		}
	case "orchestrator":
		// Orchestrator delegates to sub-agents with its own permission handling;
		// store the mode for reference but no CLI flag mapping needed.
		merged["permission-mode"] = permissionMode
	default:
		merged["permission-mode"] = permissionMode
	}

	return merged
}
