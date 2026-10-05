"""Keep an official source install private to Remotai.

Only user-wide command publication is disabled. Install-local launchers, the
official PM, backups, migrations, runtime verification and update receipts run
unchanged. The official updater re-enters multiple fresh interpreters; redirect
those exact completion scripts through this wrapper so the rule survives each
handoff. No upstream source or user PATH/profile is modified.
"""
import json
import os
from pathlib import Path
import runpy
import subprocess
import sys

root = Path(sys.argv[1]).resolve()
mode = sys.argv[2]
args = sys.argv[3:]
wrapper = str(Path(__file__).resolve())
sys.path.insert(0, str(root))

completion_files = {
    str((root / "hermes_cli/source_completion.py").resolve()),
    str((root / "hermes_cli/update_completion.py").resolve()),
}
original_popen = subprocess.Popen


class PrivatePopen(original_popen):
    def __init__(self, command, *positional, **keywords):
        if isinstance(command, (list, tuple)):
            command = list(command)
            for index, item in enumerate(command):
                if not isinstance(item, (str, os.PathLike)) or Path(item).name not in {"source_completion.py", "update_completion.py"}:
                    continue
                if str(Path(item).resolve()) in completion_files:
                    target = str(Path(item).resolve())
                    command[index:] = [wrapper, str(root), "child", target, *command[index + 1:]]
                    break
        super().__init__(command, *positional, **keywords)


subprocess.Popen = PrivatePopen

# _launchers only depends on stdlib + PM layout. Do not load the old dependency
# generation in the updater's unprepared child (which intentionally uses -S).
from hermes_cli import _launchers


def private_exposure(*unused, **ignored):
    return {"ok": True, "skipped": "remotai-private-install"}


_launchers.expose_cli = private_exposure

if mode == "child":
    target, *remaining = args
    target = str(Path(target).resolve())
    if target not in completion_files:
        raise SystemExit("Refusing a completion script outside the owned install")
    sys.argv = [target, *remaining]
    runpy.run_path(target, run_name="__main__")
elif mode == "complete":
    from pm.environments import activate_dependencies
    activate_dependencies(root)
    from hermes_cli.source_completion import complete_source_checkout
    raise SystemExit(0 if complete_source_checkout(root, desktop=False, assume_yes=True) else 1)
elif mode == "cli":
    sys.argv = [str(root / ".hermes/bin/hermes"), *args]
    # PM recovery can swap interpreters. Re-enter this adapter rather than the
    # public launcher so scoped publication stays private after the swap too.
    from hermes_cli import venv_sync
    def private_relaunch(python, project_root, argv, original, module):
        if Path(project_root).resolve() != root:
            raise RuntimeError("Refusing a runtime handoff outside the owned install")
        return [str(python), "-I", "-B", "-u", wrapper, str(root), "cli", *argv[1:]]
    venv_sync.relaunch_command = private_relaunch
    import hermes_bootstrap
    runpy.run_module("hermes_cli.main", run_name="__main__", alter_sys=True)
elif mode == "check":
    from hermes_cli._subprocess_compat import expose_pm_git
    expose_pm_git(root)
    import shutil
    git = shutil.which("git")
    if not git:
        raise SystemExit("The owned Hermes Git tool is unavailable")
    from hermes_cli.source_releases import resolve_source_target
    target = resolve_source_target("main", git_cmd=[git], cwd=root,
                                   repository="NousResearch/hermes-agent")
    commit = target.commit
    branch = target.branch or "main"
    if not commit:
        # Upstream explicitly supports main as source-branch delivery when its
        # publication record is absent. Resolve the official declared branch to
        # one exact SHA before any installer/config/database work.
        ref = "refs/heads/" + branch
        remote = subprocess.run([git, "ls-remote", "--heads",
                                 "https://github.com/NousResearch/hermes-agent.git", ref],
                                capture_output=True, text=True, encoding="utf-8", check=True)
        matches = [line.split()[0] for line in remote.stdout.splitlines()
                   if len(line.split()) == 2 and line.split()[1] == ref]
        if len(matches) != 1:
            raise SystemExit("The official Hermes source branch did not resolve to one commit")
        commit = matches[0]
    current = subprocess.run([git, "rev-parse", "HEAD"], cwd=root, capture_output=True,
                             text=True, encoding="utf-8", check=True).stdout.strip()
    print("REMOTAI_HERMES_UPDATE " + json.dumps({"available": current != commit,
                                                "version": target.version or ("main " + commit[:12]),
                                                "commit": commit, "current_commit": current,
                                                "branch": branch}))
elif mode == "revision":
    from hermes_cli._subprocess_compat import expose_pm_git
    expose_pm_git(root)
    import shutil
    git = shutil.which("git")
    if not git:
        raise SystemExit("The owned Hermes Git tool is unavailable")
    current = subprocess.run([git, "rev-parse", "HEAD"], cwd=root, capture_output=True,
                             text=True, encoding="utf-8", check=True).stdout.strip()
    print("REMOTAI_HERMES_REVISION " + current)
else:
    raise SystemExit("Unknown private install operation")
