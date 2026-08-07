# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working in this repository.

## Repository status

Runstead is currently in planning and protocol-research. The checkout contains product direction, architecture, and delivery-roadmap documentation; the Go runtime, tests, and CLI have not been bootstrapped yet. Do not assume that build, lint, test, or `runstead` commands exist until the corresponding implementation is added.

## Commands

### Inspect the repository

```sh
git status --short --branch
git log --oneline -10
git diff -- docs/ README.md CLAUDE.md
```

Read the design sources with:

```sh
less README.md
less docs/architecture.md
less docs/roadmap.md
```

### Build, test, lint, and run

There is currently no `go.mod`, `cmd/runstead`, test suite, or configured lint command, so no build/test/lint/run command is available yet. The roadmap calls for these checks once the Go CLI is introduced:

```sh
go test ./...
go vet ./...
go build ./cmd/runstead
./runstead --help
```

Use the commands only after their implementation exists. The initial protocol experiment is expected to be a reproducible `curl`/shell workflow against OmniRoute, but its scripts have not been added yet.

## Architecture

Runstead is intended to be a single Go executable implemented as a modular monolith, not a collection of services. The planned boundary is:

```text
cmd/runstead
    ↓
agent loop
    ├── protocol
    ├── provider/omniroute
    ├── tools
    ├── executor
    ├── policy
    ├── verifier
    ├── state
    └── trace
```

OmniRoute owns provider authentication, ChatGPT Web session access, transport, and provider-specific compatibility. Runstead owns task lifecycle, prompts and action protocol, local tool execution, policy, durable state, recovery, verification, and final evidence. Remote conversations are disposable; local state is authoritative.

The model proposes one action or an explicit final response. Runstead must extract and strictly validate the action, apply policy, execute the local tool, verify observable results, persist the event/checkpoint, and return a structured observation before continuing. A model claim is not evidence: filesystem state, command exit status, hashes, and Git status/diffs are the sources used for verification.

The candidate protocol is a Runstead-owned envelope in model text, initially shaped like:

```xml
<runstead_action>
{
  "tool": "read_file",
  "arguments": {"path": "README.md"}
}
</runstead_action>
```

The protocol must support deterministic extraction from mixed prose, strict JSON validation, one action per turn, explicit final responses, bounded correction retries, and versioning. Do not make implementation depend on OmniRoute's emulated native tool calling.

## Capability sequence

Work follows the gates in `docs/roadmap.md`; prove the risky protocol assumption before freezing runtime interfaces:

1. **M0 — Protocol proof:** exercise the action contract through OmniRoute and measure valid, malformed, corrected, and repeated responses.
2. **M1 — Read-only agent loop:** bootstrap the Go CLI, provider adapter, parser, read-only tools, bounded loop, and trace.
3. **M2 — Durable state and recovery:** add explicit SQLite migrations, append-oriented events, checkpoints, `inspect`, `resume`, and idempotent recovery.
4. **M3 — Safe repository modification:** add `write_file`/`apply_patch`, workspace and symlink boundaries, approvals, hashes, and diff evidence.
5. **M4 — Verifiable coding agent:** complete inspect-edit-test-fix cycles with independent acceptance checks and reject unsupported completion claims.
6. **M5 — v0.1 hardening:** exercise interruption, stale sessions, malformed responses, stuck commands, duplicate requests, and the documented end-to-end scenario.

## Design constraints

- Prefer the Go standard library and explicit mechanisms; keep external dependencies minimal.
- SQLite is the planned authoritative store for tasks, events, messages, actions, tool results, and checkpoints; use explicit SQL migrations rather than an ORM.
- Start with non-streaming OmniRoute requests and a minimal provider adapter; broad provider compatibility is deferred.
- Read-only tools come first: `read_file`, `list_files`, `search_text`, restricted `shell`, `git_status`, and `git_diff`. Write tools follow only after the read-only loop is stable.
- Enforce workspace boundaries, reject path traversal and symlink escapes, restrict shell commands, and fail closed on uncertain policy decisions.
- Keep policy decisions explicit and logged. Network access, Git commit/push, privilege escalation, destructive commands, and access outside the workspace are not automatic capabilities.
- Do not introduce agent frameworks, ORM, Redis, brokers, microservices, Kubernetes, UI, MCP, vector memory, multi-agent orchestration, or automatic model routing for v0.1; these are explicitly deferred in the roadmap.

## Preferred collaboration style

When a task benefits from parallel work, prefer using an agent team with focused workers and different model tiers. Assign routine exploration, file searches, mechanical changes, and straightforward verification to the least expensive capable model. Reserve more capable and expensive models for architectural decisions, difficult debugging, security-sensitive work, or final reviews where the additional reasoning is justified. Keep the main agent responsible for coordination, integration, and the final verification.

## Source of truth

Use `README.md` for product direction and non-goals, `docs/architecture.md` for system boundaries and runtime responsibilities, and `docs/roadmap.md` for milestone deliverables and exit criteria. When implementation decisions are not covered there, preserve the narrow ChatGPT Web-through-OmniRoute path and avoid speculative abstractions.
