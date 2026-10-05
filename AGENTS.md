# Developing Remotai

The Go agent is in cmd/tgcontrol and internal. The relay has a separate Go
module in tgcontrol-relay. The shared React client is apk/src with packages/shared.

Read README.md and docs/self-hosting.md before changing deployment behavior.
Self-hosted access must retain authentication, ownership and workspace roles.
Managed access keeps its subscription checks. Never commit credentials or data.

Use node scripts/build-self-hosted.mjs with your own relay origin to build the
embedded client and agent. Keep generated executables in build/. Do not start,
install, uninstall or reconfigure an existing agent while testing source changes.

Run npm test and the Go tests relevant to your changes. Relay checks run from
tgcontrol-relay. Go agent tests require the generated embedded client first.
