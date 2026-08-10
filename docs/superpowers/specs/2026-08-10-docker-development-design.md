# Design: constrained Docker development environment

Date: 2026-08-10
Issue: #15
Branch: `feat/issue-15-docker-dev`

## Goal

Add an optional, reproducible Docker development environment without changing
Runstead runtime architecture or making Docker authoritative. Native Go commands
remain the reference workflow.

## Chosen approach

Use one project-owned `Dockerfile.dev` and one Compose service named `dev`.
The image is based on `golang:1.22.2-bookworm`, matching the `go 1.22.2`
directive in `go.mod`, and installs only the tools required by the documented
workflow: Bash, Git, curl, jq, ripgrep, SQLite CLI, CA certificates and GCC
for the Go race detector.

The image creates `runstead` during the build. `RUNSTEAD_UID` and
`RUNSTEAD_GID` are build arguments with `1000:1000` defaults. The creation logic
reuses a pre-existing group ID when possible and fails clearly if the requested
user ID is already owned by another image user. The final image declares
`USER runstead`; there is no startup ownership repair or privileged entrypoint.
Linux users build with `RUNSTEAD_UID=$(id -u)` and
`RUNSTEAD_GID=$(id -g)` so new files in `/workspace` retain host ownership.

Compose bind-mounts only the current Runstead tree at `/workspace` by default.
It uses named volumes for `/go/pkg/mod`, `/home/runstead/.cache/go-build` and
`/home/runstead/.local/share/runstead`, exposed through `GOMODCACHE`, `GOCACHE`
and `RUNSTEAD_STATE_DIR`. `/target` is intentionally absent from the default
mount list; an operator adds one explicit bind mount with `docker compose run
-v "$RUNSTEAD_TARGET:/target"` when a target repository is needed.

The service uses normal bridge networking, drops all capabilities and enables
`no-new-privileges`. It has no Docker socket, host PID namespace, privileged
mode, host networking or broad host mounts.

OmniRoute variables are listed only as Compose passthrough entries. Values must
already exist in the operator environment at runtime. No credential is copied,
set as a Dockerfile `ARG`/`ENV`, committed, or included in examples.

## Alternatives considered

1. **Runtime UID override plus permission-fixing entrypoint.** Rejected because
   it would add startup complexity, require privileged behavior or recursive
   ownership changes, and weaken the clear non-root image contract.
2. **A separate M0 image/service.** Rejected because it duplicates the
   development environment and makes the historical disposable Dockerfile a
   second interface. The existing M0 Dockerfile remains historical, while the
   primary Compose service runs the experiment directly.
3. **Chosen build-time UID/GID mapping.** It is explicit, small and testable on
   Linux while retaining `USER runstead` in the final image.

## Validation

The implementation will run the native formatting, test, vet, build, race and
M0 commands, then the equivalent Compose commands. Static checks will inspect
Compose mounts/security settings, image user metadata, named volume mounts and
credential absence. A Linux bind-mount probe will create a file as `runstead`
and verify that the host sees the same non-root UID/GID and can edit/remove it.
