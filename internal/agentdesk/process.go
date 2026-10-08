package agentdesk

import (
	"context"
	"os/exec"

	"tgcontrol/internal/procutil"
)

func hiddenCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	return procutil.Hidden(exec.CommandContext(ctx, name, args...))
}
