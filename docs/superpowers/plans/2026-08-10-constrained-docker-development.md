# Constrained Docker Development Environment Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an optional Compose-based Runstead development environment that is reproducible, non-root, explicitly mounted, and able to run the authoritative native checks plus the M0 protocol experiment.

**Architecture:** Build one project-owned `Dockerfile.dev` from `golang:1.22.2-bookworm`, install the documented development tools, and create `runstead` with build-time `RUNSTEAD_UID`/`RUNSTEAD_GID` mapping. Expose one `dev` Compose service with `/workspace` as the only default host bind mount, named Go caches and Runstead state volumes, runtime-only OmniRoute passthrough, normal bridge networking, `no-new-privileges` and `cap_drop: ALL`. Keep `/target` absent unless the operator adds an explicit `docker compose run -v` mount.

**Tech Stack:** Dockerfile, Docker Compose Specification, Debian Bookworm APT packages, Go 1.22.2, Bash, Git, curl, jq, ripgrep, SQLite CLI, CA certificates, GCC and existing native Go/Bash commands.

## Global Constraints

- Docker is optional and native Go commands remain authoritative.
- The image must run as `USER runstead`, never UID 0.
- `RUNSTEAD_UID` and `RUNSTEAD_GID` are build args with `1000` defaults and are populated from `id -u`/`id -g` in the documented Linux flow.
- Do not add a startup entrypoint, recursive workspace `chown`, privileged mode or a runtime permission repair mechanism.
- Mount only the Runstead source at `/workspace` by default; add `/target` only through an explicit operator command.
- Use named volumes for `/go/pkg/mod`, `/home/runstead/.cache/go-build` and `/home/runstead/.local/share/runstead`.
- Do not mount `/var/run/docker.sock`, `$HOME`, `/`, broad parent directories, host PID or host networking.
- Set `no-new-privileges` and drop all capabilities in Compose.
- OmniRoute variables are runtime passthrough only; no secret values, secret defaults, credential `ARG`/`ENV`, `.env` copy or credential image layer is allowed.
- Do not change provider, governor, agent loop, persistence, verifier or protocol behavior.
- Keep `experiments/protocol/Dockerfile` as the historical M0 artifact; root Compose is the primary Docker interface.

---

### Task 1: Add the non-root development image and build-context exclusions

**Files:**
- Create: `Dockerfile.dev`
- Create: `.dockerignore`

**Interfaces:**
- Consumes: `go.mod` Go version `1.22.2` and the existing native/M0 command dependencies.
- Produces: an image whose configured user is `runstead`, whose environment contains `GOMODCACHE`, `GOCACHE`, `RUNSTEAD_STATE_DIR` and `HOME`, and whose `/workspace` and `/target` directories are ready for explicit mounts.

- [ ] **Step 1: Create `Dockerfile.dev` with the exact toolchain and UID/GID behavior**

```dockerfile
FROM golang:1.22.2-bookworm

ARG RUNSTEAD_UID=1000
ARG RUNSTEAD_GID=1000
ARG TARGETARCH=amd64

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      bash \
      ca-certificates \
      curl \
      gcc \
      git \
      passwd \
      ripgrep \
      sqlite3; \
    jq_version=1.7.1; \
    case "$TARGETARCH" in \
      amd64) jq_sha256=5942c9b0934e510ee61eb3e30273f1b3fe2590df93933a93d7c58b81d19c8ff5 ;; \
      arm64) jq_sha256=4dd2d8a0661df0b22f1bb9a1f9830f06b6f3b8f7d91211a1ef5d7c4f06a8b4a5 ;; \
      *) echo "unsupported TARGETARCH for pinned jq binary: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    curl --fail --silent --show-error --location --retry 3 \
      "https://github.com/jqlang/jq/releases/download/jq-$jq_version/jq-linux-$TARGETARCH" \
      --output /usr/local/bin/jq; \
    printf '%s  %s\n' "$jq_sha256" /usr/local/bin/jq | sha256sum --check --status; \
    chmod 0755 /usr/local/bin/jq; \
    rm -rf /var/lib/apt/lists/*; \
    case "$RUNSTEAD_UID" in \
      ''|*[!0-9]*) echo 'RUNSTEAD_UID must be a positive integer' >&2; exit 1 ;; \
    esac; \
    case "$RUNSTEAD_GID" in \
      ''|*[!0-9]*) echo 'RUNSTEAD_GID must be a positive integer' >&2; exit 1 ;; \
    esac; \
    if [ "$RUNSTEAD_UID" -eq 0 ] || [ "$RUNSTEAD_GID" -eq 0 ]; then \
      echo 'RUNSTEAD_UID and RUNSTEAD_GID must be non-zero' >&2; \
      exit 1; \
    fi; \
    if getent passwd runstead >/dev/null; then \
      existing_uid="$(getent passwd runstead | cut -d: -f3)"; \
      existing_gid="$(getent passwd runstead | cut -d: -f4)"; \
      if [ "$existing_uid" != "$RUNSTEAD_UID" ] || [ "$existing_gid" != "$RUNSTEAD_GID" ]; then \
        echo "runstead already exists with UID:GID $existing_uid:$existing_gid" >&2; \
        exit 1; \
      fi; \
      runstead_group=runstead; \
    else \
      if getent passwd "$RUNSTEAD_UID" >/dev/null; then \
        echo "requested UID $RUNSTEAD_UID is already used by another image user" >&2; \
        exit 1; \
      fi; \
      if getent group runstead >/dev/null; then \
        existing_group_gid="$(getent group runstead | cut -d: -f3)"; \
        if [ "$existing_group_gid" != "$RUNSTEAD_GID" ]; then \
          echo "runstead group already exists with GID $existing_group_gid" >&2; \
          exit 1; \
        fi; \
        runstead_group=runstead; \
      elif getent group "$RUNSTEAD_GID" >/dev/null; then \
        runstead_group="$(getent group "$RUNSTEAD_GID" | cut -d: -f1)"; \
      else \
        groupadd --gid "$RUNSTEAD_GID" runstead; \
        runstead_group=runstead; \
      fi; \
      useradd \
        --uid "$RUNSTEAD_UID" \
        --gid "$runstead_group" \
        --create-home \
        --shell /bin/bash \
        runstead; \
    fi; \
    install -d -o "$RUNSTEAD_UID" -g "$RUNSTEAD_GID" \
      /workspace \
      /target \
      /go/pkg/mod \
      /home/runstead/.cache/go-build \
      /home/runstead/.local/share/runstead

ENV HOME=/home/runstead \
    GOMODCACHE=/go/pkg/mod \
    GOCACHE=/home/runstead/.cache/go-build \
    RUNSTEAD_STATE_DIR=/home/runstead/.local/share/runstead

WORKDIR /workspace
USER runstead
CMD ["bash"]
```

- [ ] **Step 2: Create `.dockerignore` so local metadata, credentials and generated output never enter the build context**

```text
.git
.gitignore
.env
.env.*
**/.env
**/.env.*
.omx/
scratch/
experiments/protocol/results/
```

- [ ] **Step 3: Verify the new files are the only uncommitted changes**

Run: `git status --short`
Expected: only `Dockerfile.dev` and `.dockerignore` are listed.

- [ ] **Step 4: Commit the image boundary**

```bash
git add Dockerfile.dev .dockerignore
git commit -m "build(dev): add non-root development image"
```

---

### Task 2: Add the constrained Compose service

**Files:**
- Create: `compose.yaml`

**Interfaces:**
- Consumes: `Dockerfile.dev`, optional `RUNSTEAD_UID`/`RUNSTEAD_GID` shell variables, and runtime-only OmniRoute variables.
- Produces: service `dev`, named volumes `go-mod-cache`, `go-build-cache` and `runstead-state`, and no implicit `/target` mount.

- [ ] **Step 1: Create `compose.yaml` with one service, explicit volumes and security settings**

```yaml
services:
  dev:
    build:
      context: .
      dockerfile: Dockerfile.dev
      args:
        RUNSTEAD_UID: "${RUNSTEAD_UID:-1000}"
        RUNSTEAD_GID: "${RUNSTEAD_GID:-1000}"
    working_dir: /workspace
    command: ["bash"]
    stdin_open: true
    tty: true
    security_opt:
      - no-new-privileges:true
    cap_drop:
      - ALL
    environment:
      - HOME=/home/runstead
      - GOMODCACHE=/go/pkg/mod
      - GOCACHE=/home/runstead/.cache/go-build
      - RUNSTEAD_STATE_DIR=/home/runstead/.local/share/runstead
      - OMNIROUTE_BASE_URL
      - OMNIROUTE_API_KEY
      - OMNIROUTE_MODEL
      - OMNIROUTE_CHAT_ENDPOINT
    volumes:
      - type: bind
        source: .
        target: /workspace
      - type: volume
        source: go-mod-cache
        target: /go/pkg/mod
      - type: volume
        source: go-build-cache
        target: /home/runstead/.cache/go-build
      - type: volume
        source: runstead-state
        target: /home/runstead/.local/share/runstead

volumes:
  go-mod-cache:
  go-build-cache:
  runstead-state:
```

- [ ] **Step 2: Validate Compose syntax without allowing ambient credentials into output**

Run:

```bash
env -u OMNIROUTE_BASE_URL -u OMNIROUTE_API_KEY -u OMNIROUTE_MODEL -u OMNIROUTE_CHAT_ENDPOINT \
  docker compose config --quiet
```

Expected: exit code `0`, with no `privileged`, host PID, host network or socket mount introduced by interpolation.

- [ ] **Step 3: Commit the Compose boundary**

```bash
git add compose.yaml
git commit -m "build(dev): add constrained Compose service"
```

---

### Task 3: Document native authority and the Docker workflow

**Files:**
- Modify: `docs/development.md`
- Modify: `README.md` line 115, changing the Docker description from recommended to optional while preserving the existing limitations.

**Interfaces:**
- Consumes: service name `dev`, image path `Dockerfile.dev`, volume paths from `compose.yaml`, existing native command list and M0 scripts.
- Produces: copy-pasteable native and Docker commands, explicit target/credential/state instructions, cleanup guidance and honest limitations.

- [ ] **Step 1: Add the authoritative native workflow section to `docs/development.md`**

Insert after `## What Docker solves`:

```markdown
## Native workflow is authoritative

Docker is optional. The native Go workflow remains the reference path and does
not require Docker:

```bash
test -z "$(gofmt -l .)"
go test ./...
go vet ./...
go build ./cmd/runstead
go test -race ./...
bash experiments/protocol/test.sh
```

The commands above are the same checks used by CI. The M0 offline replay also
works natively:

```bash
bash experiments/protocol/run.sh --offline
```
```

- [ ] **Step 2: Add the concrete Docker image, UID/GID and shell workflow**

Insert after the native section:

```markdown
## Docker workflow

The project-owned `Dockerfile.dev` is the reproducible development image. It
uses Go `1.22.2`, installs Git, curl, jq, ripgrep, SQLite CLI, CA certificates,
Bash and GCC, and runs as the non-root `runstead` user.

On Linux, build with the host IDs so files created in the `/workspace` bind mount
remain owned by the host user:

```bash
export RUNSTEAD_UID="$(id -u)"
export RUNSTEAD_GID="$(id -g)"
docker compose build
```

If those variables are omitted, the image uses the `1000:1000` defaults. Rebuild
the image after changing the host IDs. The build fails rather than silently
reusing a conflicting UID; an existing compatible group ID is reused.

Open a development shell with:

```bash
docker compose run --rm dev bash
```

Run the native checks through Compose with:

```bash
docker compose run --rm dev go test ./...
docker compose run --rm dev go test -race ./...
docker compose run --rm dev go vet ./...
docker compose run --rm dev go build ./cmd/runstead
```

Run the M0 protocol checks through the primary development service with:

```bash
docker compose run --rm dev bash experiments/protocol/test.sh
docker compose run --rm dev bash experiments/protocol/run.sh --offline
```

The historical `experiments/protocol/Dockerfile` remains available for the M0
experiment's historical reproduction, but it is not the primary development
interface.
```
```

- [ ] **Step 3: Add explicit target repository, state, credential and cleanup documentation**

Append to the Docker workflow:

```markdown
### Target repository

No target repository is mounted by default. Select one absolute host path and
add only that path for a run:

```bash
RUNSTEAD_TARGET="$(realpath ../selected-target)"
docker compose run --rm \
  --volume "$RUNSTEAD_TARGET:/target" \
  dev bash
```

Use `:ro` in the volume specification when the target only needs to be read.
Do not replace this with a mount of `$HOME`, `/`, or a broad parent directory.

### Development state and caches

The Compose service keeps `GOMODCACHE` at `/go/pkg/mod`, `GOCACHE` at
`/home/runstead/.cache/go-build` and `RUNSTEAD_STATE_DIR` at
`/home/runstead/.local/share/runstead`. Each path is backed by a named volume.
The Runstead state volume survives `docker compose down` and container removal.

To remove containers while preserving caches and state:

```bash
docker compose down
```

To recreate the image without deleting development data:

```bash
docker compose build --no-cache
```

To reset the image, caches and Runstead development state deliberately:

```bash
docker compose down --volumes
docker compose build --no-cache
```

`docker compose down --volumes` deletes the named development state and cache
volumes.

### Runtime credentials

Offline tests require no credentials. For a live M0 attempt, the variables must
already be populated at runtime by the operator or a secret manager:

```bash
: "${OMNIROUTE_BASE_URL:?set OMNIROUTE_BASE_URL at runtime}"
: "${OMNIROUTE_API_KEY:?set OMNIROUTE_API_KEY at runtime}"
: "${OMNIROUTE_MODEL:?set OMNIROUTE_MODEL at runtime}"
docker compose run --rm dev bash experiments/protocol/run.sh --live
```

Compose only passes through those existing environment values. Do not put a key
in this document, a Dockerfile, an image build argument, a Compose default, a
fixture, a command argument, or a committed `.env` file. Avoid shell tracing
while running live mode.

### Limits

Docker is optional and exists here for reproducible dependencies and isolated
module/build caches. It is not a complete sandbox. A writable `/workspace` or
explicit writable `/target` bind mount remains modifiable by processes inside
the container. Docker does not replace Runstead policy, effect, governor or
verifier controls, and it is not a production runtime requirement or runtime
architecture boundary.
```

- [ ] **Step 4: Change the top-level README wording to state that Docker is optional**

Replace:

```markdown
A Docker-based development environment is recommended to keep Go, `jq`, test utilities, caches and later transport-specific native dependencies out of the host system.
```

with:

```markdown
An optional Docker-based development environment keeps Go, `jq`, test utilities, caches and later transport-specific native dependencies out of the host system.
```

- [ ] **Step 5: Check documentation links and whitespace**

Run:

```bash
git diff --check
```

Expected: no output and exit code `0`.

- [ ] **Step 6: Commit the documented workflow**

```bash
git add docs/development.md README.md
git commit -m "docs(dev): document native and Docker workflows"
```

---

### Task 4: Run native, Docker and invariant validation

**Files:**
- Modify: none, except temporary untracked ownership probe files that must be removed before committing.
- Inspect: `Dockerfile.dev`, `compose.yaml`, `.dockerignore`, `docs/development.md`, `README.md`.

**Interfaces:**
- Consumes: all files from Tasks 1-3 and the running Docker daemon.
- Produces: verified command results, ownership proof and a clean diff suitable for review.

- [ ] **Step 1: Run the native authoritative checks**

Run each command from the repository root:

```bash
test -z "$(gofmt -l .)"
go test ./...
go vet ./...
go build ./cmd/runstead
go test -race ./...
bash experiments/protocol/test.sh
git diff --check
```

Expected: every command exits `0`; the formatting and diff checks print nothing.

- [ ] **Step 2: Build the image with the current Linux host IDs**

Run:

```bash
export RUNSTEAD_UID="$(id -u)"
export RUNSTEAD_GID="$(id -g)"
env -u OMNIROUTE_BASE_URL -u OMNIROUTE_API_KEY -u OMNIROUTE_MODEL -u OMNIROUTE_CHAT_ENDPOINT \
  docker compose build
```

Expected: image build succeeds without credentials in the build command or image history.

- [ ] **Step 3: Run the required Compose command matrix**

Run:

```bash
env -u OMNIROUTE_BASE_URL -u OMNIROUTE_API_KEY -u OMNIROUTE_MODEL -u OMNIROUTE_CHAT_ENDPOINT \
  docker compose run --rm dev go test ./...
env -u OMNIROUTE_BASE_URL -u OMNIROUTE_API_KEY -u OMNIROUTE_MODEL -u OMNIROUTE_CHAT_ENDPOINT \
  docker compose run --rm dev go vet ./...
env -u OMNIROUTE_BASE_URL -u OMNIROUTE_API_KEY -u OMNIROUTE_MODEL -u OMNIROUTE_CHAT_ENDPOINT \
  docker compose run --rm dev go build ./cmd/runstead
env -u OMNIROUTE_BASE_URL -u OMNIROUTE_API_KEY -u OMNIROUTE_MODEL -u OMNIROUTE_CHAT_ENDPOINT \
  docker compose run --rm dev go test -race ./...
env -u OMNIROUTE_BASE_URL -u OMNIROUTE_API_KEY -u OMNIROUTE_MODEL -u OMNIROUTE_CHAT_ENDPOINT \
  docker compose run --rm dev bash experiments/protocol/test.sh
```

Expected: every command exits `0`; M0 prints its existing `PASS` line.

- [ ] **Step 4: Prove the non-root user and host ownership contract**

Run from the repository root:

```bash
probe=".docker-ownership-probe.$$"
trap 'rm -f "$probe"' EXIT
env -u OMNIROUTE_BASE_URL -u OMNIROUTE_API_KEY -u OMNIROUTE_MODEL -u OMNIROUTE_CHAT_ENDPOINT \
  docker compose run --rm dev sh -c "printf 'created-in-container\\n' > /workspace/$probe"
expected="$(id -u):$(id -g)"
actual="$(stat -c '%u:%g' "$probe")"
test "$actual" = "$expected"
printf 'edited-on-host\\n' >"$probe"
env -u OMNIROUTE_BASE_URL -u OMNIROUTE_API_KEY -u OMNIROUTE_MODEL -u OMNIROUTE_CHAT_ENDPOINT \
  docker compose run --rm dev sh -c "test \"\$(cat /workspace/$probe)\" = edited-on-host"
rm -f "$probe"
trap - EXIT
```

Expected: the container creates the file with the host UID/GID, the host edits it, and the container reads the host edit. No probe file remains.

Also run:

```bash
env -u OMNIROUTE_BASE_URL -u OMNIROUTE_API_KEY -u OMNIROUTE_MODEL -u OMNIROUTE_CHAT_ENDPOINT \
  docker compose run --rm dev sh -c 'test "$(id -u)" -ne 0; id'
```

Expected: the command prints `uid=...` with a non-zero UID.

- [ ] **Step 5: Prove named volumes, mounts and security invariants from resolved Compose metadata**

Run:

```bash
config_json="$(env -u OMNIROUTE_BASE_URL -u OMNIROUTE_API_KEY -u OMNIROUTE_MODEL -u OMNIROUTE_CHAT_ENDPOINT docker compose config --format json)"
printf '%s\n' "$config_json" | jq -e '
  (.services.dev.privileged // false) == false and
  (.services.dev.pid // "") != "host" and
  (.services.dev.network_mode // "") != "host" and
  (.services.dev.security_opt | index("no-new-privileges:true") != null) and
  (.services.dev.cap_drop | index("ALL") != null) and
  ([.services.dev.volumes[].source] | all(test("docker.sock|^/$|^/home/[^r]"; "i") | not)) and
  ([.services.dev.volumes[] | select(.type == "volume") | .target] | sort ==
    ["/go/pkg/mod", "/home/runstead/.cache/go-build", "/home/runstead/.local/share/runstead"]) and
  ([.services.dev.volumes[].target] | index("/target") == null)
'
container_id="$(docker compose create dev)"
docker inspect "$container_id" --format '{{.Config.User}}'
docker inspect "$container_id" --format '{{json .Mounts}}' | jq -e '
  map(select(.Type == "volume") | .Destination) | sort ==
  ["/go/pkg/mod", "/home/runstead/.cache/go-build", "/home/runstead/.local/share/runstead"]
'
docker rm "$container_id" >/dev/null
```

Expected: JSON assertions pass; the configured image user is `runstead`; the three cache/state destinations are named volumes; there is no `/target` default mount, Docker socket, root/home broad mount, privileged mode, host PID or host networking.

- [ ] **Step 6: Prove credentials are absent from source configuration and image metadata**

Run without ambient OmniRoute values:

```bash
env -u OMNIROUTE_BASE_URL -u OMNIROUTE_API_KEY -u OMNIROUTE_MODEL -u OMNIROUTE_CHAT_ENDPOINT \
  docker compose config --format json | jq -e '
    (.services.dev.environment | index("OMNIROUTE_API_KEY") != null) and
    (.services.dev.environment | index("OMNIROUTE_BASE_URL") != null)
  '
image_id="$(docker compose images -q dev)"
test -n "$image_id"
docker image inspect "$image_id" --format '{{json .Config.Env}}' | jq -e 'map(test("OMNIROUTE_API_KEY|OMNIROUTE_BASE_URL|OMNIROUTE_MODEL") | not) | all'
docker history --no-trunc "$image_id" | grep -Eiq 'sk-|Bearer[[:space:]]|OMNIROUTE_API_KEY=.*[^$]' && exit 1 || true
```

Expected: Compose contains only passthrough variable names, the image environment contains no OmniRoute variables or values, and image history contains no credential-shaped value.

- [ ] **Step 7: Remove temporary resources and review the final diff against main**

Run:

```bash
docker compose down
git status --short
git diff --stat origin/main...HEAD
git diff --check origin/main...HEAD
git diff --name-only origin/main...HEAD
```

Expected: no ownership probe or local secret file is present; only the intended Docker files, documentation and committed design/plan artifacts differ from `origin/main`; diff check passes. Do not run `docker compose down --volumes` unless intentionally resetting the named state/cache volumes.

- [ ] **Step 8: Commit any validation-only cleanup, then prepare PR body**

If validation produced no tracked changes, no additional commit is needed. Verify the branch contains the design commit plus the three implementation commits and push it:

```bash
git log --oneline --decorate origin/main..HEAD
git push -u origin feat/issue-15-docker-dev
```

Open a non-draft PR targeting `main` with title:

```text
build(dev): add constrained Docker development environment
```

The body must contain `Closes #15` and summarize architecture, mounts/volumes, UID/GID/non-root behavior, credential policy, security limits, every native/Docker command run and any Docker command that could not run. Do not manually close the issue.
