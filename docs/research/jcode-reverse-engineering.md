# JCode Reverse Engineering Report

**Date:** 2026-08-06
**Author:** Research agent (Jcode)
**Status:** Complete
**Purpose:** Evidence-based reverse engineering of JCode v0.70.1 and its comparison with Runstead, as a basis for architectural decisions and future issues. No functional changes, no copied code, no PR, no commit.

---

## 1. Executive summary

This report is a static-analysis-driven reverse engineering of JCode
(commit `435fb4a83bee429762acd1cc905ba9987bff65d7`, tag `v0.70.1`) and a
systematic comparison with the current Runstead tree
(commit `56d0aa9c5ff79bf68dd1735fd01442668e7a97a4`). JCode is a ~700k-line
Rust workspace with 83 crates (697,955 measured lines of Rust) and 6,810 commits; Runstead is a ~4k-line Go
stdlib-only modular monolith with 17 commits. They are at opposite maturity
points and opposite architectural philosophies. The value of JCode for
Runstead is not its feature set, but a small set of **durability, recovery,
strictness and test-design ideas** that can be ported as concepts or algorithms.

> **Resumo executivo (PT-BR):** O JCode é um harness de agente maduro, monolítico
> por dentro, com persistência snapshot+journal tolerante a corrupção, classificação
> de risco de comando por "blast radius", compactação de contexto com corte seguro
> de pares tool-call/result, e testes de alta qualidade sobre invariantes de
> cancelamento e recuperação. Sua maior fraqueza arquitetural é a ausência de um
> boundary de efeitos fail-closed: a execução de `bash` só é protegida por um gate
> opcional e heuristicamente contornável, e o modelo pode declarar sucesso sem
> verificação externa. Para o Runstead, o valor está em ~10 ideias transplantáveis
> (conceito ou algoritmo), não em código. Nenhuma parte do JCode deve ser copiada
> diretamente; a recomendação final é: **adotar as ideias de journal+checkpoint,
> observações truncadas explícitas, fingerprints canônicos, classificação por blast
> radius, testes de invariantes de cancelamento/replay e presets de perfis de
> orçamento, adaptando-as ao protocolo, ao governor e ao verifier do Runstead.**

### 1.1 The 10 most valuable ideas found

| # | Idea | Where | Verdict |
|---|------|-------|---------|
| 1 | Snapshot + append-only JSONL journal with **corrupt-line salvage and checkpoint-after-corruption** | `jcode-base/src/session/persistence.rs` | ADAPTAR AGORA (as invariants, against SQLite) |
| 2 | **Blast-radius command risk classification** (Safe/Low/Confirm/Catastrophic) instead of a denylist | `crates/jcode-command-risk/src/lib.rs` | ADAPTAR AGORA |
| 3 | Canonical JSON + SHA-256 **action fingerprint** for repeated-action detection | Runstead already has `ActionFingerprint` (`internal/protocol/parser.go:423`) | JCode confirms the pattern; Runstead already owns it |
| 4 | **Explicit, never-silent truncation** with original/returned counts and `untrusted` markers | `crates/jcode-tool-types`, Runstead `docs/tools.md` | Runstead already implements it better |
| 5 | Tool schema central injection of `intent` (why this call is being made) | `crates/jcode-tool-core/src/lib.rs:9-26` | ADAPTAR (as a protocol/prompt contract, not a schema hack) |
| 6 | `effective_context_tokens_from_usage` provider-accounting normalization | `crates/jcode-compaction-core/src/lib.rs:362-387` | ADIAR (needs token accounting) |
| 7 | `safe_compaction_cutoff` keeps tool-call/result pairs together | `crates/jcode-compaction-core/src/lib.rs:238-291` | ADIAR (compaction milestone) |
| 8 | Interrupt signal with fire **epoch** (`reset_if_epoch`) so stale cancels never erase newer cancels | `crates/jcode-agent-runtime/src/lib.rs:33-118` | ADAPTAR AGORA (Go: `context` + epoch counter) |
| 9 | Emergency compaction paths that **replace dropped content with explicit markers** instead of silent loss | `crates/jcode-compaction-core/src/lib.rs:449-692` | ADIAR |
| 10 | Budget/profile presets as **explicit local operating ceilings** (Instant/Reasoning profiles) | `docs/account-protection.md` (Runstead) vs JCode pricing/route tables | Runstead's governor already surpasses JCode here |

### 1.2 Best 3 for immediate adoption

1. **Journal/snapshot durability invariants** (salvage torn lines, checkpoint after corruption, `write_json_fast` vs `write_bytes` distinction) — directly informs Runstead's future SQLite event store and its append-oriented event history.
2. **Blast-radius risk classification + two-stage gate** (deterministic assess, then a reflection turn for `Confirm`, absolute deny for `Catastrophic`) — directly informs Runstead's future write-tool and shell policy boundary, preserving fail-closed.
3. **Interrupt/epoch semantics and turn-cancellation tests** — informs the Go `runstead run` signal handling and cancellation tests for the future agent loop.

### 1.3 Best 3 for future adoption

1. **Compaction cutoffs that preserve tool-call/result pairing and never drop silently** — for the milestone that introduces context reconstruction.
2. **Provider error taxonomy and streaming event normalization** (`StreamEvent` with `RetryRollback`, `SessionId`, `TokenUsage`) — for the Runstead provider interface when streaming arrives.
3. **Response-recovery heuristics** (`recover_text_wrapped_tool_call`, stop-reason classification, guardrail notices) — as inspiration for the Runstead parser's bounded correction policy, *not* as code.

### 1.4 The 5 that must be rejected

1. **The multi-client server/socket architecture** (Unix socket daemon, session picker, hot reload). Conflicts with "local-first, remote sessions disposable" and with the single-binary modular monolith.
2. **Embedding/vector memory graph** with sideagents, consolidation and semantic injection. Adiado by Runstead; adds RAM, staleness and injection risk.
3. **Provider multi-routing with fallback chains, model catalogs, pricing tables and automatic route selection**. Would turn Runstead's narrow provider seam into a generic router.
4. **Swarm, ambient/overnight autonomy, notifications, email, desktop app** — out of scope for the current stage.
5. **The justification-reflection gate for destructive commands** (a model-provided justification can unlock `Confirm`). Runstead must keep an external human approval gate, not a model self-justification gate.

### 1.5 Strengths and weaknesses

- **Greatest architectural strength of JCode:** the session persistence model (snapshot + journal + corruption salvage + crash recovery) and the honest, well-tested invariant engineering around cancellation (`InterruptSignal` epoch tests, `fire_never_loses_wakeup`).
- **Greatest architectural weakness of JCode:** execution safety is not structural. The `bash` tool historically ran `rm -rf ~` immediately (documented in `crates/jcode-command-risk/src/lib.rs:1-10` as issue #604); the destructive gate is per-tool, opt-in-ish and unlockable by a model-provided `justification`. The model can also declare completion without external verification. Runstead's governor/protocol/verifier model is strictly stronger.
- **Greatest opportunity for Runstead:** borrow the *recovery and strictness testing discipline* (corrupt journal replay, torn-write salvage, crash detection, cancel races) and apply it to the SQLite event store, the action parser and the future loop — while keeping the governor as the only authorization boundary.
- **Greatest risk of copying JCode:** importing its "tool calling as the contract" mindset (native tool calls, per-tool gates, model-controlled schema fields, prompt-dependent safety) would undermine `runstead.protocol.v1`, the fail-closed governor and the verifier. Also, copying its 83-crate structure would destroy the modular-monolith goal.
- **Final recommendation in one sentence:** Adopt only the *concepts* of journal-style recovery, blast-radius risk classification, epoch-based cancellation and strict truncation/evidence accounting from JCode, reimplemented independently in Go behind the Runstead protocol, governor and verifier; reject everything that makes the model the authority over effects or turns Runstead into a router/framework.

---

## 2. Repositories and analyzed commits

### 2.1 JCode

| Field | Value |
|---|---|
| URL | https://github.com/1jehuang/jcode |
| Default branch | `master` |
| Commit analyzed | `435fb4a83bee429762acd1cc905ba9987bff65d7` |
| Commit date | 2026-08-05 21:00:35 -0700 |
| Version/tag | `v0.70.1` (release commit `chore(release): v0.70.1`) |
| Language / toolchain | Rust, edition 2024, tokio async runtime; workspace of 83 crates |
| License | MIT (root `LICENSE`, Copyright (c) 2025 Jeremy Huang) |
| Size | 209 MB checkout with `.git` (442 MB `.git`); source-only much smaller |
| Files | 1,890 tracked files; 1,237 `.rs` files; 36 tracked files under `tests/` (9 top-level entries); 273 test-related paths |
| Commits | 6,810 |
| Working tree status | Clean clone; no local modifications |

### 2.2 Runstead

| Field | Value |
|---|---|
| URL | https://github.com/RenyEnnos/Runstead (private; API 404 for anonymous access) |
| Branch (local worktree) | `RenyEnnos/ladyfish` |
| Commit analyzed | `56d0aa9c5ff79bf68dd1735fd01442668e7a97a4` |
| Commit date | 2026-08-03 21:35:13 -0400 |
| Version/tag | none (pre-v0.1) |
| Language / toolchain | Go 1.22.2, zero external dependencies (`go.mod` has no requires) |
| License | no LICENSE file yet (not assessed) |
| Files | 102 tracked files; 131 Go test functions |
| Commits | 17 |
| Working tree status | Clean (`git status --short` empty) |

Note: Runstead's GitHub API returned 404 for issues, PRs and repo metadata
(`https://api.github.com/repos/RenyEnnos/Runstead/issues`), so the "issues"
used in this report are those **referenced by the code and docs**
(#2, #3, #4, #5, #6, #7, #8, #13, #14, #15, #16, #17, #20, #21, #22, #24, #28, #29, #30)
and reconstructed from the git history. This is listed again under limitations.

---

## 3. Scope and limitations

### 3.1 In scope

- Full static map of the JCode workspace (crates, responsibilities, dependency direction, composition root).
- Reconstruction of the main flows: binary startup, agent turn, tool call, provider, session/server, persistence.
- Deep analysis of 13 subsystems (agent runtime, tools, safety, providers, sessions/server, storage, compaction, memory, TUI, harness API, swarm/ambient, performance, tests).
- Systematic comparison matrix with Runstead, phased integration plan, issue backlog.
- Controlled local verification where the environment allowed it.

### 3.2 Limitations (explicit)

1. **No Rust toolchain available on the analysis machine** (`cargo`, `rustc` and `rustup` are not installed; `command -v cargo` returned nothing). Therefore **Fase 4 dynamic verification of JCode could not be executed**: no `cargo metadata`, `cargo check`, or `cargo test` results exist in this report. All JCode evidence is static (code, docs, tests read from the tree) and marked accordingly. This does not hide the fact: it is stated here and in section 33.
2. **Runstead GitHub issues/PRs are not readable** (private repo, anonymous API 404). Issue numbers were reconstructed from commit messages and docs. The state of issues #29/#30 ("authoritative attempt receipts") is *declared in docs* as future work; the code confirms the adapter is fail-closed until then (`internal/provider/omniroute/client.go:208-213`).
3. **No live provider calls were made** (per instructions). No credentials were touched.
4. **JCode README performance claims** (RAM, time-to-first-frame) are treated as *declared claims*. The methodology artifacts exist (`scripts/bench_startup.py`, `scripts/bench_startup_visible_ready.py`, `scripts/memory_probe.sh`, `scripts/memory_regression_gate.sh`, `scripts/run_terminal_bench_campaign.py`) and were inspected, but the numbers themselves were **not reproduced** on this machine.
5. Deep-dive reads were prioritized: files cited with line ranges were read; some very large files (e.g. `jcode-tui`'s 201k lines, `jcode-desktop2`) were inspected structurally (function inventory, module lists) rather than line-by-line.
6. The report distinguishes evidence levels everywhere: **Confirmado no código**, **Confirmado por teste** (tests read statically), **Declarado apenas na documentação**, **Inferência arquitetural**, **Não foi possível verificar**.

### 3.3 Evidence legend

Used throughout:

- `C` = **Confirmado no código** (read directly in the analyzed commit).
- `T` = **Confirmado por teste** (test code read in the analyzed commit; not executed).
- `D` = **Declarado apenas na documentação** (README/docs claim, not verified in code).
- `I` = **Inferência arquitetural** (reasonable inference from code structure).
- `NV` = **Não foi possível verificar** (missing toolchain/credentials/network).

---

## 4. Methodology

1. Snapshot both repositories: `git rev-parse HEAD`, `git log -1 --format=fuller`, `git ls-files`, `git status --short`, `git rev-list --count HEAD`, `git tag`.
2. Read the entire Runstead documentation set and the complete Go source tree (it is small enough to read fully: `cmd/runstead`, `internal/agent`, `internal/config`, `internal/governor`, `internal/protocol`, `internal/provider`, `internal/provider/omniroute`, `internal/tools`, `internal/trace`).
3. Run the Runstead test suite (`go test ./...`) to confirm the analyzed state.
4. Clone JCode to `/tmp/jcode-re`; read `Cargo.toml`, `src/main.rs`, `src/lib.rs`, the architecture RFCs (`docs/MODULAR_ARCHITECTURE_RFC.md`, `docs/CRATE_OWNERSHIP_BOUNDARIES.md`, `docs/SERVER_ARCHITECTURE.md`, `docs/SAFETY_SYSTEM.md`, `docs/BROWSER_PROVIDER_PROTOCOL.md`), the README, and the key crates (provider-core, tool-core, command-risk, compaction-core, agent-runtime, session persistence, storage, harness-api, swarm-core, memory-types, session-types).
5. Reconstruct flows by reading the actual call sites (`turn_loops.rs`, `turn_execution.rs`, `response_recovery.rs`, `persistence.rs`, `storage/lib.rs`, tool registry).
6. Attempt dynamic verification; record the toolchain absence honestly (section 33).
7. Build the comparison matrix, integration plan and issue backlog against the Runstead constraints (sections 25-31).

---

## 5. JCode overview

### 5.1 Product shape (declared)

From `README.md` and `Cargo.toml:4`: "Possibly the greatest coding agent ever built — blazing-fast TUI, multi-model, swarm coordination, 30+ tools". JCode is a terminal-first coding agent harness supporting multiple providers (Claude/Anthropic OAuth+API, OpenAI OAuth+API, OpenRouter, Gemini, Copilot, Cursor, Bedrock, Antigravity, Claude Code CLI bridge, OpenAI-compatible profiles), a single-server/multi-client architecture over Unix sockets, sessions persisted to disk, a memory graph with local ONNX embeddings, swarm coordination, ambient/overnight autonomy, self-dev (the agent modifies its own harness), a desktop app (`jcode-desktop2`, wgpu/Vello), a harness API + TypeScript SDK, and an auto-updater.

### 5.2 Observed shape (code)

- The **root crate is a thin CLI/composition shell**: `src/main.rs` (236 lines) builds a tokio runtime and calls `jcode::run()`; `src/lib.rs` (31 lines) re-exports `jcode_tui::*` (which re-exports `jcode-app-core` and `jcode-base`) plus `cli`. C `src/lib.rs:22-26`.
- The real product logic lives in three giant crates: `jcode-base` (106,428 lines), `jcode-app-core` (134,150 lines), `jcode-tui` (201,930 lines). C (line counts measured with `wc -l` over `git ls-files`). Total workspace: 697,955 lines of `.rs` (`git ls-files '*.rs' | xargs wc -l`).
- The workspace has 83 crates (82 members under `crates/` plus the root package); most are small `*-types` DTO crates plus provider leaf crates and TUI leaf crates. C `Cargo.toml:8-93` (82 member lines with `crates/` prefix), `ls crates | wc -l` = 83 directories (one of them, `jcode-math`, is not a member; see section 6.1).
- The architecture is a **modular monolith in the middle of a layered-crate migration**; the RFC itself says "today, jcode is best described as a modular monolith with a growing workspace shell" and the root crate still owns most orchestration. D/C `docs/MODULAR_ARCHITECTURE_RFC.md:31-36`.
- The composition root is `src/cli/startup.rs` (`jcode::run()` -> `cli::startup::run()`), reached through `src/main.rs:139`. I.

### 5.3 What JCode is NOT

- It is **not** a fail-closed execution boundary. `bash` execution is gated only by an opt-in `pre_tool` hook and, since the destructive-gate work, a per-tool deterministic assessor whose `Confirm` tier can be unlocked by a model-provided justification. C `crates/jcode-command-risk/src/lib.rs:1-33`, `crates/jcode-app-core/src/tool/bash_destructive_gate.rs:8-33`.
- It is **not** a verifier: tool outputs are appended to the conversation as-is; task completion is the model's claim (a final text answer). I (no verification subsystem found; `verify`/`verifier` symbols absent from the tree inventory).
- It is **not** a durable-task store in the Runstead sense: sessions are persisted (snapshot+journal), but the "task" concept is the conversation, and provider session ids are recorded as resumable state. C `jcode-base/src/session.rs:93-108` (Session struct stores `provider_session_id`).

---

## 6. JCode crate map and dependencies

### 6.1 Crate families (83 crates, grouped)

Measured with `wc -l` over tracked files per crate (full table in section 25 matrix; families here).

**Composition root and CLI**
- `jcode` (root, 236+31 lines: `main.rs` + `lib.rs`, plus `src/cli/*`), bins: `jcode`, `test_api`, `jcode-harness`, dev bins (`session_memory_bench`, `memory_recall_bench`, `mermaid_side_panel_probe`, `tui_bench`). C `Cargo.toml:99-129`.

**Domain/runtime core (the real product)**
- `jcode-base` (222 files, 106,428 lines): session, persistence, compaction manager, memory, provider registry/state, storage helpers, config, protocol values, tools' data, auth. C.
- `jcode-app-core` (254 files, 134,150 lines): agent loop (`agent/`), server (`server/`, 80+ files), tool registry and implementations (`tool/`), ambient, restart snapshot, replay, TUI-independent product glue. C.

**Agent runtime primitives**
- `jcode-agent-runtime` (298 lines): `InterruptSignal` (epoch-based), `SoftInterruptMessage`, `StreamError`. C.

**Provider core and adapters**
- `jcode-provider-core` (7,312 lines): the `Provider` trait, `EventStream`, `ModelRoute`/`RuntimeKey`/`RouteSelection`, pricing, failover classification, auth-mode vocabulary, shared HTTP client. C.
- Provider leaf crates: `jcode-provider-{anthropic,openai,openrouter,gemini,bedrock,copilot,antigravity}` (schema/catalog helpers) and `-runtime` crates (`anthropic-runtime` 4,672, `openai-runtime` 8,387, `openrouter-runtime` 7,952, `gemini-runtime` 2,655, `copilot-runtime` 1,900, `cursor-runtime` 1,330, `antigravity-runtime` 1,586, `claude-cli-runtime` 1,167), plus `jcode-provider-metadata` (1,853), `jcode-provider-env` (426), `jcode-provider-doctor` (6,783). C.

**Tools and permissions**
- `jcode-tool-core` (303 lines): `Tool` trait, `ToolContext`, intent schema injection. `jcode-tool-types` (158): `ToolOutput`. Tool implementations live in `jcode-app-core/src/tool/` (~50 files incl. `bash`, `edit`, `write`, `read`, `agentgrep`, `websearch`, `browser`, `batch`, `bg`, `mcp`, `memory`, `patch`, `apply_patch`, `multiedit`, `communicate`, `session_search`, `todo`, `goal`...). TUI permission reviewer in `jcode-tui-permissions` (885). C.

**Command risk and safety**
- `jcode-command-risk` (1,822 lines): deterministic blast-radius classifier + two-stage gate. C.
- `docs/SAFETY_SYSTEM.md` (Design status): review queue for ambient autonomy. D.

**Protocols and DTOs**
- `jcode-protocol` (5,162 lines): client/server wire protocol, `AuthChanged`, `SwarmMemberStatus`, `TranscriptMode`, etc. C.
- 14 `*-types` crates plus `jcode-plan` and `jcode-schema-dialect`: message-types (906), session-types (1,077), tool-types, task-types (828), plan (5,815), side-panel-types (101), memory-types (1,996), config-types (2,505), auth-types (188), background-types (160), batch-types (46), gateway-types (27), ambient-types (41), usage-types (958), selfdev-types (201), schema-dialect (2,676). C.

**Session and server**
- Session domain inside `jcode-base/src/session/` (`journal.rs`, `persistence.rs`, `crash.rs`, `model.rs`, `render.rs`, `storage_paths.rs`, `maintenance.rs`, `memory_profile.rs`). Server domain inside `jcode-app-core/src/server/` (80+ files: sockets, client lifecycle, swarm, reload, debug socket, durable state). C.

**Persistence**
- `jcode-storage` (1,180 lines): atomic JSON writes (durable vs fast), `.bak` hard-link recovery, append-only JSONL line append, secret file permission hardening, active PID tracking, runtime dirs. C.

**Memory and embeddings**
- `jcode-memory-types` (1,996): `MemoryEntry` with confidence/trust/tags, pipeline state. `jcode-embedding` (717): ONNX MiniLM via `tract`, HF tokenizer. Memory orchestration in `jcode-base/src/memory*` (memory.rs, memory_graph.rs, memory_agent.rs, memory_rerank.rs, memory_log.rs...). C.

**Compaction**
- `jcode-compaction-core` (1,045): budget constants, cutoffs, summary prompt, emergency truncation. Manager in `jcode-base/src/compaction.rs` (~800+ lines). C.

**TUI and rendering**
- `jcode-tui` (201,930), `jcode-tui-core` (3,218), `jcode-tui-render` (5,601), `jcode-tui-markdown` (9,688), `jcode-tui-mermaid` (11,548), `jcode-tui-style` (5,134), `jcode-tui-messages` (1,148), `jcode-tui-anim` (1,149), `jcode-tui-session-picker` (304), `jcode-tui-account-picker` (1,498), `jcode-tui-workspace` (1,235), `jcode-tui-tool-display` (244), `jcode-tui-usage-overlay` (944), `jcode-tui-visual-debug` (874), `jcode-tui-permissions` (885). C.

**Side panels, Markdown, Mermaid**
- `jcode-side-panel-types` (101), `jcode-tui-markdown`, `jcode-tui-mermaid` (custom pure-Rust mermaid renderer, claimed 1800x faster than browser-based; `docs/MERMAID_RENDERING_REDESIGN.md`). D/C.

**Harness API, bridge, SDK**
- `jcode-harness-api` (1,714): versioned NDJSON frame protocol (`v` major, `id`/`reply_to`), `HarnessClient`. `jcode-harness-api-server` (4,062): Unix-socket server shipped inside the binary as `jcode api-bridge`. `jcode-sdk` (4,412): TypeScript SDK package sources. C.

**Telemetry, update, distribution**
- `jcode-telemetry-core` (4,519), `jcode-update-core` (632), `jcode-build-meta` (476), `jcode-build-support` (3,263), `jcode-logging` (1,281), `jcode-notify-email` (558), `jcode-terminal-launch` (1,739), `jcode-terminal-image` (731). C.

**Ambient, overnight, productivity, self-dev**
- `jcode-ambient-types` (41), `jcode-overnight-core` (1,481), `jcode-productivity-core` (1,751), `jcode-selfdev-types` (201), `jcode-setup-hints` (16,259). C.

**Heavy/optional integrations**
- `jcode-embedding` (tract/ONNX, feature-gated), `jcode-pdf` (63, feature-gated), `jcode-azure-auth` (24), `jcode-desktop2` (42,918: winit/wgpu/Vello desktop), `jcode-fuzzy` (813, per-keystroke matcher). Note: `crates/jcode-math` (12,140 lines, animation kernels) is **not a workspace member**: `comm -3 <(ls crates) <(Cargo.toml members)` shows it as the only directory absent from the members list, so it is not compiled by the workspace. C.

### 6.2 High-level dependency graph

Observed dependency direction (RFC-consistent, confirmed by reading `Cargo.toml` of the root and the crate `use` statements):

```mermaid
flowchart TD
  BIN[jcode bin: main.rs] --> LIB[jcode lib: cli + re-export]
  LIB --> TUI[jcode-tui 201k lines]
  LIB --> CLI[src/cli/*]
  TUI --> APP[jcode-app-core 134k lines]
  APP --> BASE[jcode-base 106k lines]
  APP --> TOOLCORE[jcode-tool-core]
  APP --> RISK[jcode-command-risk]
  BASE --> STORAGE[jcode-storage]
  BASE --> COMPACT[jcode-compaction-core]
  BASE --> PROVCORE[jcode-provider-core]
  BASE --> MEMORY[jcode-memory-types]
  PROVCORE --> PROVRT[jcode-provider-*-runtime]
  PROVCORE --> PROVLEAF[jcode-provider-{openai,anthropic,...}]
  BASE --> TYPES[jcode-*-types 15 crates]
  APP --> PROTOCOL[jcode-protocol]
  APP --> HARNESS[jcode-harness-api / -server]
  APP --> SWARM[jcode-swarm-core]
  APP --> PLAN[jcode-plan]
  TUI --> TUIL[jcode-tui-* leaves]
  APP --> DESKTOP[jcode-desktop2]
  APP --> EMB[jcode-embedding]
  BIN --> HARNESS
```

Notes: `jcode-tui` re-exports `jcode-app-core` and `jcode-base` (`src/lib.rs:22`), so the "layered" crate split is partially cosmetic: the three giant crates are one compile unit in practice (`cargo check -p jcode --bin jcode` compiles all of them). C `src/lib.rs:22`, `crates/jcode-tui/src/lib.rs` (re-export chain confirmed by reading the root lib).

### 6.3 Composition root, coupling hotspots, volatility

- **Composition root:** `src/main.rs` + `src/cli/startup.rs` (`jcode::run()` -> `cli::startup::run()`). C `src/main.rs:139`, `src/lib.rs:29-31`.
- **Biggest coupling hotspots:** `jcode-base`, `jcode-app-core`, `jcode-tui` (the three giant crates); the RFC explicitly names `src/server`, `src/session`, `src/provider/mod.rs`, `src/tui` as chokepoints. D/C `docs/MODULAR_ARCHITECTURE_RFC.md:144-156`.
- **Stable vs volatile:** stable = `*-types` DTO crates, `jcode-agent-runtime`, `jcode-tool-core`, `jcode-command-risk`, `jcode-compaction-core` (leaf, few deps). Volatile = `jcode-app-core` agent/server, `jcode-tui` app state, provider runtimes, `jcode-desktop2`.
- **Real vs pretended boundaries:** real = type crates, command-risk, compaction-core, storage, agent-runtime (small public APIs, no upward deps). Pretended = the "layered" RFC target (server/agent/session/provider crates do not exist; the RFC itself is a plan, status Draft). C `docs/MODULAR_ARCHITECTURE_RFC.md:3`.

---

## 7. Observed architecture

### 7.1 Product architecture: single server, multi client

```mermaid
flowchart TD
  subgraph Server process (jcode serve)
    SOCK[Unix socket /run/user/$UID/jcode.sock]
    DBG[Debug socket]
    SRV[Server orchestration]
    SES[Sessions on disk: snapshot + journal]
    PROV[Provider registry + runtimes]
    MEM[Memory graph + embeddings]
    SW[Swarm coordination]
    BG[Background tasks]
  end
  C1[TUI client 1] <--> SOCK
  C2[TUI client 2] <--> SOCK
  HARNESS[Harness API server api-bridge] <--> SOCK
  CLI[jcode CLI] -->|spawn if missing| SRV
  SRV --> PROV -->|EventStream| PROVIDERS[Anthropic/OpenAI/OpenRouter/Gemini/...]
  SRV --> SES
  SRV --> MEM
  SRV --> SW
```

C `docs/SERVER_ARCHITECTURE.md:14-33`, `crates/jcode-app-core/src/server/` (80+ files).

### 7.2 Layered target vs observed reality

- Target (RFC, Draft): foundation -> domain -> interface -> composition, with `jcode-server`, `jcode-agent`, `jcode-session`, `jcode-provider` crates. D `docs/MODULAR_ARCHITECTURE_RFC.md:220-292`.
- Reality: root = CLI shell; product = three giant crates (`base`, `app-core`, `tui`) plus ~87 leaf crates; the RFC's target crates **do not exist**. C (crate inventory), D `docs/MODULAR_ARCHITECTURE_RFC.md:31-36`.
- The dependency guard (`scripts/check_dependency_boundaries.py`) enforces that `*-types` crates do not depend on runtime crates. C (script present; its enforcement is code, the CI wiring is inferred).

### 7.3 Where state lives

- Session state: `Session` struct in `jcode-base/src/session.rs` (messages, compaction state, provider_session_id, model, env snapshots, memory injections, replay events). C `jcode-base/src/session.rs:93+`.
- Server state: client lifecycle, swarm, background tasks, durable state in `jcode-app-core/src/server/state.rs`, `durable_state.rs`. C.
- Provider state: `provider` module in `jcode-base/src/provider/` (registry, state). C (module listing).
- Tool registry: `jcode-app-core/src/tool/mod.rs` + implementations. C.

### 7.4 Invariants that JCode actually protects (and those it does not)

Protected (with tests):
- A cancel must never be lost (`InterruptSignal::fire` race hammer, 2000 iterations). T `crates/jcode-agent-runtime/src/lib.rs:180-207`.
- A stale cancel must never erase a newer cancel (`reset_if_epoch`). T `crates/jcode-agent-runtime/src/lib.rs:234-259`.
- A journal replay must not truncate the transcript at the first corrupt line. T (replay tests in `jcode-base/src/session/persistence.rs` test module; salvage logic present). C/T.
- Compaction must never leave a kept tool result without its tool use. T `crates/jcode-compaction-core/src/lib.rs:803-833`.
- Token accounting must not double-count provider cache counters. T `crates/jcode-compaction-core/src/lib.rs:722-761`.

NOT protected (design-level):
- The model's claimed completion is not externally verified. I, strengthened by C: a workspace-wide grep for `fn verify|struct Verifier|trait Verifier|verify_completion|verify_action` finds matches only in `crates/jcode-update-core/src/lib.rs` and `crates/jcode-app-core/src/update.rs` (binary signature verification) and `crates/jcode-base/src/auth/copilot.rs` (credential verification); no task/completion verifier exists.
- A destructive shell command is blocked only by a per-tool assessor whose `Confirm` tier accepts a model justification. C `crates/jcode-command-risk/src/gate.rs:78-99`.
- Tool outputs are trusted as conversation content and fed back verbatim (subject only to char caps). C `turn_loops.rs` (`cap_tool_output_for_history`).

---

## 8. Initialization flow

Reconstructed from `src/main.rs`, `src/cli/startup.rs`, `docs/SERVER_ARCHITECTURE.md`, `crates/jcode-app-core/src/server/server_spawn.rs`:

1. `main()`: configure allocator (jemalloc optional or glibc `mallopt` tuning), install rustls crypto provider. C `src/main.rs:105-140`.
2. Intercept special invocations before tokio (setup-hotkey callbacks, macOS hotkey listener, notification broker). C `src/main.rs:114-133`.
3. Build multi-thread tokio runtime; `runtime.block_on(jcode::run())`. C `src/main.rs:135-139`.
4. `cli::startup::run()` parses CLI args (clap) and dispatches. I (from module structure and `src/lib.rs:29-31`).
5. **Server check/spawn:** if no server registered in `~/.jcode/servers.json` and no socket, spawn `jcode serve` detached (`setsid`), wait for socket; otherwise connect. C/D `docs/SERVER_ARCHITECTURE.md:53-69`.
6. **Session create/resume:** client creates or resumes a session; session is loaded from disk (snapshot + journal replay) or created. C `docs/SERVER_ARCHITECTURE.md:64-66`, `jcode-base/src/session/persistence.rs:175-241`.
7. Provider/auth initialization: provider registry builds runtimes for configured credentials (`src/cli/provider_init.rs`, `src/cli/startup.rs`). C (module listing).
8. TUI renders first frame; startup profile logged (used by `bench_startup.py`). C `scripts/bench_startup.py:24-34`.

Crash recovery on startup: `recover_crashed_sessions()` scans for crashed sessions via PID files and journal markers. C `jcode-base/src/session/crash.rs:14-23`.

---

## 9. Agent turn flow

Source: `crates/jcode-app-core/src/agent/turn_loops.rs` (`run_turn`, 1,260 lines), `turn_execution.rs`, `streaming.rs`, `response_recovery.rs`, `provider.rs`.

### 9.1 Step-by-step (blocking loop, `run_turn`)

1. Mark streaming; register turn cancel signal in `turn_cancel_registry`. C `turn_loops.rs:34-47`.
2. Loop head: bail if graceful shutdown observed. C `turn_loops.rs:57-63`.
3. `repair_missing_tool_outputs()` recovers missing tool results before the API call. C `turn_loops.rs:60-68`.
4. Build provider messages via `messages_for_provider()`; if compaction event fired, reset cache tracker and tool lock. C `turn_loops.rs:67-79`.
5. Build tool definitions (locked snapshot for cache stability; one-shot MCP late-registration recheck). C `turn_loops.rs:85-93`, `turn_execution.rs:344-399`.
6. Non-blocking memory prompt build (uses last turn's pending result, spawns next check). C `turn_loops.rs:88-90`.
7. Split system prompt (static vs dynamic) for cache. C `turn_loops.rs:92-93`.
8. Inject memory as a trailing user message; optional batch nudge. C `turn_loops.rs:97-122`.
9. `provider.complete_split(...)` -> `EventStream`; on error, try `try_auto_compact_after_context_limit` with bounded retries (`MAX_CONTEXT_LIMIT_RETRIES`). C `turn_loops.rs:133-169`.
10. Stream loop consumes `StreamEvent`s: text deltas, thinking, tool-use start/input deltas/end, tool results (SDK-executed), token usage, session id, stop reason, `RetryRollback` (mid-stream replay discards partial output), native tool calls, compaction events, errors. C `turn_loops.rs:180-660`.
11. After stream: filter truncated tool calls when stop reason indicates truncation (`filter_truncated_tool_calls`). C `turn_loops.rs:813-821`.
12. If no tool calls: guardrail reconsideration (`maybe_reconsider_fable_guardrail`), empty-post-tool continuation, incomplete-response continuation with bounded counters; otherwise turn ends. C `turn_loops.rs:832-860`.
13. Tool execution: **sequential loop** over tool calls; `validation_error()`, `validate_tool_allowed(name)`, SDK-executed results honored for non-native tools; local execution via `registry.execute(name, input, ctx)`. Results appended as `ToolResult` messages; session saved when dirty. C `turn_loops.rs:916-1130`.
14. Soft interrupts injected for next turn. C `turn_loops.rs:1131+`.
15. Repeat loop until no tool calls; `final_text` returned.

### 9.2 Structural findings

- **The loop is explicit but single-purpose** (`run_turn`); variants exist (`run_turn_streaming_mpsc`, headless, `turn_streaming_mpsc.rs` 1,730 lines). The RFC flags "agent turn-loop unification" as open work. D `docs/MODULAR_ARCHITECTURE_RFC.md:580-587`.
- **No external verification of completion**: "turn complete - no tool calls" is the termination criterion; the model's text is the final answer. C `turn_loops.rs:871`.
- **Iteration limits**: context-limit compaction retries (bounded), incomplete continuations (bounded), empty-post-tool continuations (bounded), guardrail reconsiderations (bounded). C `turn_loops.rs:44-52`.
- **Cancellation**: `InterruptSignal` (flag+notify+epoch), `GracefulShutdownSignal`, `BackgroundToolSignal`, soft interrupts; turn registry for session-level cancels. C `crates/jcode-agent-runtime/src/lib.rs`, `crates/jcode-app-core/src/agent/interrupts.rs`.
- **Persistence points**: user message appended and `session.save()` before turn; assistant message + tool results saved during/after the turn; crash window exists between effects and saves (acknowledged design). C `turn_execution.rs:6-35`, `turn_loops.rs:890-910`.
- **Provider retry**: no retry in the provider itself; the *turn* retries after auto-compaction or on `StreamEvent::Error` with `retry_after_secs`; transport retries exist at runtime level (fresh client per retry to avoid poisoned pools). C `crates/jcode-provider-core/src/lib.rs:645-666`.
- **Empty/truncated handling**: stop-reason classification (`should_continue_after_stop_reason`), text-wrapped tool call recovery (`recover_text_wrapped_tool_call`), whitespace-only final answers not persisted. C `response_recovery.rs:56-120`, `turn_loops.rs:757-764`.

---

## 10. Tool call flow (representative: `read`)

1. Model emits `ToolUseStart`/`ToolInputDelta`/`ToolUseEnd` stream events; input accumulated as JSON text and parsed to an object; `intent` extracted. C `turn_loops.rs:308-390`.
2. Tool call validated (`validation_error()` for malformed input), tool allowed check (`validate_tool_allowed`). C `turn_loops.rs:920-940`.
3. `ToolContext` built: session_id, message_id, tool_call_id, working_dir, stdin channel, graceful shutdown signal, execution mode. C `crates/jcode-tool-core/src/lib.rs:102-141`.
4. `registry.execute(name, input, ctx)` dispatches to the implementation. C `crates/jcode-app-core/src/tool/mod.rs` (registry), implementations under `tool/read/`.
5. Result (`ToolOutput { output, title, ... }`) capped by `cap_tool_output_for_history`; converted to `ToolResult` content blocks appended as a User message; session saved. C `turn_loops.rs:1045-1098`.
6. Errors become `ToolResult { is_error: Some(true) }` messages. C `turn_loops.rs:1099-1125`.

### 10.1 Registry-level protections (C)

- Context guard withholds oversized tool results and states their token cost; `accept_large_output: true` re-runs deliberately. C `crates/jcode-app-core/src/tool/mod.rs:733-848`, `crates/jcode-tool-core/src/lib.rs:12-18`.
- Every tool schema gets a required `intent` property centrally (why this call is being made). C `crates/jcode-tool-core/src/lib.rs:44-92`.
- The `bash` tool has the destructive gate (section 15).
- Tool output caps, truncation helpers. C.

### 10.2 Gaps vs Runstead's model (C/I)

- No **typed failure taxonomy** for tools: errors are `anyhow::Error` strings (`Result<ToolOutput>`), unlike Runstead's typed `Failure` codes. C `crates/jcode-tool-core/src/lib.rs:145-166`.
- No **repeated-action guard** at the registry: repetition is prevented by prompt nudges (batch nudge, sequential-tool-round counters), not by fingerprinting. C `turn_loops.rs:905-915` (`update_sequential_tool_rounds`).
- No **permission check before every effect** by default: only the opt-in `pre_tool` hook (off by default) and the bash gate. C `crates/jcode-app-core/src/tool/mod.rs:657-679`.
- No **workspace boundary** on file tools in the JCode core (the `read`/`write` tools operate on paths the model chooses; no canonical workspace jail found in tool-core). I (not found in inventory; Runstead's `internal/tools/workspace.go` is stricter).

---

## 11. Provider flow

### 11.1 The contract

- `Provider` trait (`crates/jcode-provider-core/src/lib.rs:76-487`): `complete(messages, tools, system, resume_session_id) -> Result<EventStream>` plus 57 methods total (auth labels, model routing, reasoning effort, compaction hooks, `fork`). Counted with `awk` over the trait block.
- `EventStream = Pin<Box<dyn Stream<Item = Result<StreamEvent>> + Send>>`; `StreamEvent` covers Thinking*, TextDelta, ToolUse*, ToolResult, TokenUsage, SessionId, MessageEnd, RetryRollback, ConnectionType/Phase, StatusDetail, Error, NativeToolCall, OpenAIReasoning, Compaction, GeneratedImage, UpstreamProvider. C `crates/jcode-provider-core/src/lib.rs:71-72`, `crates/jcode-message-types` (StreamEvent definition).
- Auth: dual-mode vocabulary (`AuthMode`, `AuthRoute`, `CredentialMode::Auto/OAuth/ApiKey`, `ResolvedCredential`); runtime env pins; OAuth flows in `src/cli/login/`. C `crates/jcode-provider-core/src/auth_mode.rs`, `src/cli/login.rs`.
- Model catalog: `ModelRoute`, `RuntimeKey`, `RouteSelection`, pricing/cheapness estimates, `context_limit_for_model_with_provider`. C `crates/jcode-provider-core/src/lib.rs:675-1248`.
- Errors: provider-specific `Error` types per runtime; transport classification `is_transient_transport_error`; failover classifier (`classify_failover_error_message`), `retry_after` parsing. C `crates/jcode-provider-core/src/transport.rs`, `failover.rs`, `retry_after.rs`.

### 11.2 Streaming normalization highlights (C)

- `RetryRollback { attempt, max }`: provider replays mid-stream; jcode discards partial output to avoid duplication. C `turn_loops.rs:487-511`.
- `SessionId` event updates `provider_session_id` (both agent and session). C `turn_loops.rs:527-535`.
- `TokenUsage` feeds the compaction manager's observed-token feed. C `turn_loops.rs:434-470`.
- Native tool calling bridge: `NativeToolCall` events execute local tools and return results through `native_result_sender`. C `turn_loops.rs:570-620`.

### 11.3 Assessment for Runstead

- The provider contract is **wide** (57 methods) and provider-routing-heavy; Runstead's one-call `Client.Complete` (Go, `internal/provider/provider.go:103-105`) is deliberately the opposite. Do not widen the Go interface.
- Useful ideas: (a) the `RetryRollback` semantics (a transport replay must not duplicate effects) map to Runstead's idempotency requirement; (b) the error taxonomy (transport vs auth vs rate vs malformed) matches what Runstead already does in `omniroute/errors.go`; (c) `fresh_transport_client` on retry (avoid poisoned connection pools) is a Go `http.Transport` concern for later; (d) session-id as disposable metadata is already Runstead doctrine.

---

## 12. Sessions and server

### 12.1 Session lifecycle (C)

- `Session` (id, title, messages, compaction state, provider_session_id, provider_key, model, route, reasoning effort, env snapshots, memory injections, replay events, status). C `jcode-base/src/session.rs:93+`.
- Persistence: full snapshot JSON + append-only journal (section 14). C `jcode-base/src/session/persistence.rs`.
- Startup stub loading for remote clients (metadata-only fast path). C `persistence.rs:253-258`.
- Crash recovery: `recover_crashed_sessions()` via PID files and session status. C `jcode-base/src/session/crash.rs`.
- Rewind (`/rewind N`) truncates messages and resets provider session; undo snapshot kept in memory. C `turn_execution.rs:200-245`.

### 12.2 Server (C/D)

- Single server, multiple clients over a Unix socket (`/run/user/$UID/jcode.sock`) + debug socket; clients reconnect with exponential backoff (1s..30s); server idle-shutdown after 5 min; `/reload` execs a new binary on the same socket; server name registry `~/.jcode/servers.json`. C/D `docs/SERVER_ARCHITECTURE.md`.
- Client attach/detach, lightweight control, comm channels (swarm messaging), file-activity change notifications ("code shifting under its feet"), durable swarm state, background tasks, debug socket commands. C `crates/jcode-app-core/src/server/` module list.
- **Runstead relevance:** this entire layer conflicts with the Runstead principle "remote sessions are disposable; local state is authoritative" and with a single-binary CLI. The *only* transferable ideas are (a) process-local cancellation registry keyed by session/turn, and (b) reconnect/resume semantics as inspiration for `runstead resume`, but implemented as process restart + SQLite, not as a daemon.

---

## 13. Persistence and recovery

This is the single most valuable JCode subsystem for Runstead.

### 13.1 Storage primitives (`crates/jcode-storage/src/lib.rs`, C)

- `write_bytes_inner`: temp file + rename; optional fsync (`durable`), optional owner-only permissions (`secret`); on Unix keeps a `.bak` hard link to the previous inode so readers never see ENOENT. C `storage/lib.rs:518-601`.
- `write_json` (durable) vs `write_json_fast` (atomic rename, no fsync; safe against process crash, not power loss). C `storage/lib.rs:486-499`.
- `append_json_line_fast`: one `O_APPEND` write of the complete serialized line + newline; prevents torn interleaving between concurrent appenders. C `storage/lib.rs:663-685`.
- `read_json` with `.bak` recovery on corruption. C `storage/lib.rs:613-661`.
- Secret hardening: owner-only dir/file permissions, Windows ACL worker. C `storage/lib.rs:225-398`.

### 13.2 Session journal (`jcode-base/src/session/persistence.rs`, C)

- Snapshot = full serialized `Session`; journal = JSONL of `SessionJournalEntry { meta, append_messages, append_env_snapshots, append_memory_injections, append_replay_events }`. C `persistence.rs:131-140`.
- Replay tolerates corrupt lines: `replay_journal_lines` skips bad lines and continues; `salvage_glued_journal_entries` recovers complete entries from torn/glued lines (writer died mid-append; next append glued to the same line). C `persistence.rs:26-129`.
- After a corrupt journal, `schedule_checkpoint_after_corrupt_journal` forces the next `save()` to write a full snapshot (deleting the bad journal, keeping a `.corrupt.jsonl` forensic copy). C `persistence.rs:151-173`.
- Journal is deleted after a checkpoint snapshot. C `persistence.rs:142-149`.

### 13.3 Crash recovery (`jcode-base/src/session/crash.rs`, C)

- PID-file based detection of crashed sessions (`is_pid_running`), crash-group retention by timestamp, resume lookup by name/id. C `crash.rs:14-210`.

### 13.4 Idempotency, dedup, transactions

- **No transactions** (no SQLite/DB); atomic rename provides per-file atomicity. I.
- **No request idempotency keys** in persistence (client request IDs exist only for the session-picker flows in some providers). I.
- Deduplication: session search scores, not write dedup. I.

### 13.5 Runstead translation

Runstead's M2 milestone (SQLite event store) should adopt these *invariants*, not the file format:
1. Append-oriented history: events are immutable appends; derived status is updatable but reconstructible. (Runstead already declares this in `docs/architecture.md`.)
2. Torn-write tolerance: with SQLite, use WAL + explicit transactions; simulate torn writes in tests and assert replay completeness (mirror `salvage_glued_journal_entries`).
3. Checkpoint-after-corruption: periodic compaction/checkpoint of the event log; keep forensic copy.
4. Durable vs fast writes: distinguish "must survive power loss" (checkpoint, fsync) from "must survive process crash" (WAL commit without fsync) in the Go layer.
5. `ClientRequestID` (Runstead governor) already provides exact-request suppression; SQLite should persist completed IDs for restart-safe dedup (currently in-memory, per `docs/account-protection.md`).

---

## 14. Security and permissions

### 14.1 What exists (C)

1. **Destructive-command gate for `bash` only** (`crates/jcode-command-risk`, wired at `crates/jcode-app-core/src/tool/bash_destructive_gate.rs`):
   - Stage 1 `assess(command, ctx)` tokenizes, unwraps wrapper programs (`sudo`, `env`, `xargs`, `timeout`...), recognizes shells (`sh -c "..."` recursion), destructive verbs (`rm`, `shred`, `dd`, `mkfs`...), conditionally destructive flags (`find -delete`, `git clean`, `chmod -R`), truncating redirects (`>`), and pipe-fed destructive commands. Classifies **by blast radius**: `Safe` (run), `Low` (bounded, run+record), `Confirm` (irreversible/outside workdir -> reflection), `Catastrophic` (home/root/credentials -> absolute deny). C `crates/jcode-command-risk/src/lib.rs:44-69,146-361`.
   - Stage 2 `gate(assessment, justification)`: `Confirm` requires a substantive justification (>= 25 chars, not an empty affirmation); `Catastrophic` is `Deny` regardless. C `crates/jcode-command-risk/src/gate.rs:78-122`.
   - Honest limitations in its own docs: "defense in depth, not a sandbox"; `sh -c "$(printf ...)"` can defeat any static parser. C `crates/jcode-command-risk/src/lib.rs:27-33`.
2. **Opt-in `pre_tool` hook** (external policy hook, off by default). C `crates/jcode-app-core/src/tool/mod.rs:657-679`.
3. **Credential hygiene**: secret file permission hardening, redacted config strings, owner-only runtime dirs, external auth file validation (no symlinks). C `crates/jcode-storage/src/lib.rs:225-432`.
4. **OAuth flows** with device/browser codes (`src/cli/login/`, `OAUTH.md`). D/C.
5. **`docs/SAFETY_SYSTEM.md`** (Design status): a two-tier permission system (auto vs requires-permission) with a persistent review queue for ambient mode. **Not confirmed implemented as a general gate**; only ambient is the documented consumer. D.

### 14.2 What does NOT exist (structural gaps, C/I)

- **No fail-closed default for shell**: the bash gate runs *after* the tool is called; safe commands run without any permission check; the gate is bypassable by design for `Confirm` with a model justification.
- **No per-effect permission check** before every side effect (no governor equivalent). The `pre_tool` hook is the only hook and is opt-in.
- **No workspace jail** for file tools (paths are model-chosen).
- **No repeated-action rejection** (only prompt nudges).
- **No verification** that claimed effects happened.
- **No path traversal/symlink escape checks** in the core tool layer (JCode's `read` accepts absolute paths; there is no canonical-workspace resolver like Runstead's `internal/tools/workspace.go`).

### 14.3 Verdict for Runstead

- **Reject** the justification-unlockable model (a model self-justification is not a human approval; Runstead's approval gates must remain human/CLI-owned).
- **Adopt the blast-radius classification idea** as a deterministic pre-check inside a future *shell* policy boundary, but with Runstead's policy semantics: `Safe` -> allow only if policy says so; everything else -> deny or human approval. Never model-unlockable.
- The tokenizer/wrapper-unwrapping algorithm (handling `sudo`/`env`/`xargs`/`sh -c` recursion and `>file` truncation) is the useful, portable core. Port the *algorithm* (independent Go implementation), not the code.
- Runstead already has the stronger structural guarantees (workspace boundary, typed failures, repeat guard, fail-closed governor). C `internal/tools/workspace.go`, `internal/protocol/parser.go`, `internal/governor/`.

---

## 15. Compaction and context management

### 15.1 Design (C)

- Budgets: `DEFAULT_TOKEN_BUDGET` 200k; trigger at 80%; **critical synchronous hard-compact at 95%**; manual compact above 10%; keep 10 recent turns verbatim; minimum 2 turns in emergency; `SYSTEM_OVERHEAD_TOKENS` 18k; image charged flat 1,600 tokens (not base64 length). C `crates/jcode-compaction-core/src/lib.rs:5-75`.
- Manager (`jcode-base/src/compaction.rs`): dirty char tracking, token snapshots, embedding snapshots for semantic triggers, anti-signal blocking, proactive and semantic triggers, observed-input-token feed from provider usage. C `compaction.rs:134+`, `should_compact_proactively` (line 514), `should_compact_semantic` (line 561).
- **Provider accounting normalization**: `effective_context_tokens_from_usage` distinguishes split accounting (Anthropic-style: `input + cache_read + cache_creation`) from subset accounting (OpenAI-style: `prompt_tokens` already includes cache). C `compaction-core/src/lib.rs:362-387`.
- **Safe cutoff**: `safe_compaction_cutoff` grows the kept suffix until every kept tool result has its matching tool use; if impossible, does not compact. C `compaction-core/src/lib.rs:238-291`.
- **Emergency paths are never silent**: dropped messages produce an `[Emergency compaction]` summary with counts, tool names and file mentions; oversized tool results are truncated head+tail with an explicit `... [N chars truncated] ...` marker; oversized images are replaced by a text marker stating media type and original size. C `compaction-core/src/lib.rs:449-692`.
- Summary format: natural-language sections (Context / What we did / Current state / User preferences) plus "you can search the full conversation later". C `compaction-core/src/lib.rs:77-85`.
- Native provider compaction (OpenAI encrypted artifact) is stored and replayed. C `compaction-core/src/lib.rs:89-94`, `turn_loops.rs:806-812`.

### 15.2 Risks observed (C/I)

- Summaries are model-generated; requirement loss is possible. JCode mitigates with explicit markers and searchability, not with verifiable requirement preservation. I.
- Emergency truncation of tool results can hide evidence the verifier would need. I (JCode has no verifier, so this is acceptable there; it would be dangerous in Runstead).

### 15.3 Runstead translation (ADIAR for now)

- Compaction is milestone C for Runstead. When it arrives, adopt: (1) trigger thresholds as explicit config, (2) **pair-preserving cutoff**, (3) **explicit markers instead of silent drops**, (4) the summary-section format, (5) the provider accounting rule as a documented heuristic. Do not adopt model-generated summaries as authoritative state; Runstead's `FinalResponse.Evidence` must remain verifiable.

---

## 16. Memory and embeddings

### 16.1 What exists (C/D)

- `MemoryEntry` with confidence, trust levels, tags, reinforcement, decay, searchable text; `PipelineState`, `MemoryEventKind`. C `crates/jcode-memory-types/src/lib.rs`.
- Local embeddings: ONNX MiniLM via `tract` + HF tokenizer (`crates/jcode-embedding`, feature-gated; ~87 MB model, per `src/main.rs` comments). C.
- Memory graph + search + rerank + extraction sideagent (`jcode-base/src/memory*.rs`); memory injected as a trailing user message; consolidation via ambient mode. C/D `README.md` (Memory section), code module listing.
- Costs acknowledged in code: embedding model load/unload is a RAM spike (jemalloc tuning comments reference ~87 MB model and 1.4 GB RSS history). C `src/main.rs:5-19`.

### 16.2 Runstead classification

- **ADIADO (vector memory)** — already a Runstead non-goal. The only ideas worth keeping for *context durability without vectors*:
  - Memory entries with explicit source, timestamp, trust and staleness fields -> maps to Runstead's future "working summary" entity in SQLite.
  - Injection as a clearly-marked trailing message, not a rewrite of history -> maps to Runstead's bounded-context reconstruction, which must keep the original objective and constraints authoritative.
  - Consolidation as a separate review pass (not inline) -> maps to checkpoint maintenance, not to model-time injection.
- Risks documented for the record: stale memories, injection of irrelevant or outdated context, RAM cost of local inference, and the fact that memory content is model-generated (untrusted). D/C.

---

## 17. TUI, rendering, side panels

### 17.1 What exists (C/D)

- `jcode-tui` (201,930 lines): app state, reducers, ratatui rendering, streaming without flicker (claimed 1000+ fps), custom scrollback, side panels, mermaid rendering via a custom pure-Rust renderer (`jcode-tui-mermaid`, 11,548 lines), markdown rendering (`jcode-tui-markdown`), info widgets, usage overlays, workspace map, session/account pickers, visual debug, animation kernels (`jcode-tui-anim`; `jcode-math` exists but is not a workspace member), emoji/color config.
- Harness-side "evidence presentation": tool cards with status/intent (`ToolEvent`), usage display, session search UI, diff viewing in side panel. C/D.
- Performance engineering is elaborate: `opt-level=3` pins for ratatui/unicode/cosmic-text/wgpu in dev profiles, allocation tuning, idle-draw cost scripts. C `Cargo.toml:285-770`.

### 17.2 Runstead relevance

- A TUI is an explicit non-goal. The only transferable *ideas* for Runstead's CLI trace output:
  1. **Tool status lifecycle** (Running -> Completed/Error) with intent and title -> Runstead's `internal/trace` JSON output should carry the same shape (action id, status, duration).
  2. **Explicit truncation markers** in rendered output (already in Runstead's observation model).
  3. **Startup profile instrumentation** (logged milestones, budgets) -> a cheap, high-value pattern for Runstead's CLI (log phase timings for `run`).
  4. Budget-gated scripts (`check_startup_budget.sh`) as a CI pattern.

---

## 18. Harness API, bridge and SDK

### 18.1 What exists (C)

- `jcode-harness-api`: versioned NDJSON-over-Unix-socket protocol; every frame has `v` (major), `id`/`reply_to`; clients must ignore unknown fields and unknown event kinds (forward compatibility); additive changes bump `API_VERSION_MINOR`, breaking changes bump `API_VERSION_MAJOR` with handshake negotiation. C `crates/jcode-harness-api/src/lib.rs:1-100`.
- `jcode-harness-api-server`: embedded in the released binary as `jcode api-bridge` (Unix-only). C `Cargo.toml:237-243`.
- `jcode-sdk`: TypeScript SDK package. C (crate listing).
- Schema snapshot tests + capability coverage tests. C (test module paths in `harness_api/src/lib.rs:35-47`).

### 18.2 Runstead assessment

- The **frame-versioning discipline** (major/minor, unknown-field tolerance, `id`/`reply_to`) is a good pattern for the *internal* `runstead.protocol.v1` versioning, but Runstead's protocol is a model-facing text protocol, not a client/server wire protocol. Keep them separate.
- Introducing a public harness API now is premature: no server, no multi-client, and Runstead's `inspect`/`resume` CLI is the correct interface for the current stage. **ADIAR/REJEITAR for now.**

---

## 19. Swarm, ambient, overnight, productivity

### 19.1 Swarm (C/D)

- Server-owned swarm coordination: member status, DMs/broadcasts over comm channels, file-activity notifications ("code shifting under its feet"), plan/task graphs (`jcode-plan`, `jcode-swarm-core`), task DAGs with gates, worker lifecycle, `swarm` CLI action vocabulary. C module inventory; D `docs/SWARM_ARCHITECTURE.md`, `docs/SWARM_TASK_GRAPH.md`.
- Cost: substantial server state, channels, persistence (`swarm_persistence.rs`), coordination tests.

### 19.2 Ambient/overnight (C/D)

- `jcode-ambient-types` (usage/rate-limit records), `jcode-overnight-core` (1,481 lines), scheduler + runner in `jcode-app-core/src/ambient*`. Safety review queue (Design status). D/C.

### 19.3 Runstead classification

- **REJEITAR/ADIAR**: multi-agent, unattended autonomy and prolonged background operation are explicit non-goals. The only *small primitive* worth noting is the swarm **tldr rule** (`validate_swarm_tldr`: messages over 240 chars must carry a one-line tldr) — a cheap presentation/observability convention that could inspire Runstead's final evidence report formatting. Not a priority.

---

## 20. Performance and efficiency

### 20.1 Claims vs evidence

| Claim | Status |
|---|---|
| "Most RAM efficient harness": 27.8 MB PSS (1 session, embeddings off), 117 MB (10 sessions) | **NV (not reproduced)**. Methodology artifacts exist: `scripts/memory_probe.sh`, `scripts/memory_regression_gate.sh` (idle rss_anon bound 55 MiB, live heap bound 45 MiB), README's own table with versions. The README numbers describe `jcode v0.9.1888-dev` while the analyzed commit is v0.70.1; numbers are dated and environment-specific. D/NV. |
| Time to first frame 14 ms; first input 48.7 ms | **NV**. Methodology: `scripts/bench_startup_visible_ready.py`, `bench_startup.py` (PTY launches, built-in startup profile, budgets: cold total <= 150 ms, server-ready <= 80 ms). Reproducible in principle on this repo, but no Rust toolchain here. D/NV. |
| Mermaid rendering 1800x faster | **D** (README + separate repo `mermaid-rs-renderer`); not assessed. |
| "renders at over a thousand fps" | D; TUI code exists, not measured here. |

### 20.2 Engineering practices that are real (C)

- **Allocator tuning**: optional jemalloc with tuned decay/arenas; glibc fallback with `mallopt` arena cap and mmap threshold pinning to return freed memory to the OS. C `src/main.rs:1-78`.
- **Compile profiles**: `release` uses `opt-level=1`, `codegen-units=256`, incremental; `release-lto` for distribution; `selfdev` profile; per-crate `opt-level=3` pins for hot render/compute stacks. C `Cargo.toml:285-770`.
- **Feature flags** isolate heavy stacks: `pdf`, `embeddings`, `bedrock`, `jemalloc`, `dev-bins`, `linux-compat-vendored-openssl`. C `Cargo.toml:208-235`.
- **Startup budgets** enforced by scripts. C `scripts/check_startup_budget.sh`, `bench_startup.py --check`.
- **Fresh HTTP client on transport-fault retry** (no pooled reuse) and HTTP/2 keepalive pings. C `crates/jcode-provider-core/src/lib.rs:606-666`.
- **Incremental session memory**: per-session ~10 MB extra PSS (declared), runtime memory log tooling (`scripts/jcode_memory_snapshot.py`, `analyze_runtime_memory_log.py`). D/C.

### 20.3 Go translation

- Go: use the standard allocator; the relevant principles are: (a) avoid loading heavy optional subsystems unless used (feature flags / build tags), (b) bounded buffers everywhere (Runstead already does), (c) `http.Transport` with sane keepalives + fresh transport on retry, (d) startup/phase timing instrumentation + budgets in CI. Do not replicate jemalloc/opt-level engineering without a measured need.

---

## 21. Tests and reliability engineering

### 21.1 Inventory (C)

- 36 tracked files under `tests/` (9 top-level entries) + 273 test-related paths across crates; extensive in-module `#[cfg(test)]` suites (e.g. `command-risk` has 4 test files, `compaction-core` has inline tests, `agent-runtime` has race hammers).
- Notable categories observed:
  - **Race/cancel invariant tests**: `fire_never_loses_wakeup` (2000 iterations), `reset_if_epoch_never_erases_concurrent_fire`. T `crates/jcode-agent-runtime/src/lib.rs:180-282`.
  - **Persistence/recovery tests**: journal replay with corrupt lines, salvage, `.bak` recovery, env-file injection rejection. T `crates/jcode-storage/src/lib.rs:748-769`, `jcode-base/src/session/persistence.rs`.
  - **Protocol/schema snapshot tests**: harness API schema snapshots. T `crates/jcode-harness-api/src/lib.rs:35-47`.
  - **Provider matrix tests**: `tests/provider_matrix.rs`, `tests/context_window_matrix.rs` (asserts shared context-window resolution invariants without live providers). T.
  - **Tool corpus tests**: `webfetch_corpus_tests.rs`, agentgrep tests, bash gate tests. T.
  - **Live/credential-dependent tests** exist but are gated by env (`real_provider_smoke.sh`, `live_tests.rs`); `find_unlocked_env_tests.py` exists to find tests that would run live accidentally. C.
  - **Stress/e2e**: `stress_test.py`, `tests/e2e/`, `test_e2e.sh`, `test_swarm.py`, selfdev reload tests. C.
- Budget enforcement: `check_code_size_budget.py`, `check_test_size_budget.py`, `check_panic_budget.py`, `check_swallowed_error_budget.py` (error-handling quality gates), `check_warning_budget.sh`, `check_dependency_boundaries.py`. C (scripts present).

### 21.2 Patterns Runstead can adopt immediately

1. **Race-hammer tests for cancellation** (loop N times, assert invariant). Directly applicable to the future Go loop (`context` + epoch cancel): a test that fires cancel concurrently with the loop's wait, asserting no lost cancel and no double-execution.
2. **Corrupt-input replay tests**: feed torn/glued/corrupt lines and assert replay completeness. Directly applicable to the action parser (already strict) and the future SQLite store (simulated torn transactions).
3. **Schema snapshot tests**: freeze protocol JSON shapes (Runstead's `runstead.protocol.v1` envelopes) and fail on accidental drift. Cheap, high value.
4. **Matrix tests without live providers**: Runstead's `internal/provider/fake.go` already provides this; extend the pattern with fixture corpora (already exists in `experiments/protocol/fixtures/`).
5. **Budget gates in CI** (`check_*_budget.py` pattern): Runstead CI currently runs `go test`; adding a small size/complexity budget per package is cheap.
6. **Error-handling budget** (`check_swallowed_error_budget.py`): enforces that errors are not swallowed; Runstead's typed failures and `go vet`/`errcheck`-style discipline align.

---

## 22. Divergences between documentation and code

1. **"Layered workspace" RFC vs reality**: RFC is Draft; server/agent/session/provider crates do not exist; three giant crates carry the product. C/D.
2. **README "most intelligent harness" / benchmarks**: promotional; numbers correspond to older versions and were not reproduced. D/NV.
3. **Safety System doc (Design) vs code**: no general permission gate found; only the bash destructive gate (post-hoc) and opt-in `pre_tool` hook. D/C.
4. **"30+ tools"**: confirmed (tool inventory in `jcode-app-core/src/tool/` has 30+ implementations including MCP proxies). C.
5. **Native tool-calling independence**: JCode depends heavily on native tool calling (`StreamEvent::ToolUse*`) as the primary contract; text-wrapped recovery is a fallback. This is the opposite of Runstead's text-protocol-first decision; JCode's approach is fine for its model, inadequate as a model for Runstead. C.
6. **Server doc naming ("🔥 blazing 🦊 fox")**: cosmetic; real.
7. **Storage claims**: "Data is still safe against process crashes" for `write_json_fast` — confirmed by design (atomic rename), with the caveat that a crash between the effect and the save still loses the *effect record* (session-level, acknowledged). C/I.

---

## 23. Comparison with Runstead (direct)

| Dimension | JCode (v0.70.1) | Runstead (56d0aa9) |
|---|---|---|
| Language | Rust 2024, tokio, 83 crates | Go 1.22, stdlib only, modular monolith |
| Authority over effects | Model proposes + executes via tools; gates are per-tool and partial | Model proposes; Runstead validates, authorizes, executes, verifies (`docs/architecture.md`) |
| Protocol | Native tool calling primary; text-wrapped recovery fallback | `runstead.protocol.v1` text envelopes primary; independent of tool calling |
| Completion truth | Model's final text / no tool calls | `runstead_final` + verifier; evidence required; false claims rejected |
| State | Sessions on disk (snapshot+journal), no SQLite | SQLite planned as authoritative task store (M2) |
| Safety | Bash gate (blast radius) + opt-in hook; no governor | Account governor (FIFO lane, budgets, circuit, fail-closed) + strict parser + workspace jail |
| Retries | Turn-level retries after compaction/errors; provider runtime retries with fresh client | One call/one attempt; governor owns retry *eligibility*; no autonomous retry |
| Multi-client | Unix-socket server, reconnect, reload | Single process, single binary; remote sessions disposable |
| Memory | Vector graph + embeddings + sideagents | Deferred (non-goal) |
| TUI | Massive (201k lines) | CLI only (trace JSON) |
| Tests | 273+ test paths, race hammers, budgets | 131 test functions, focused per package |
| Maturity | v0.70.1, 6,810 commits | pre-v0.1, 17 commits |

---

## 24. Complete component matrix

Columns: Componente / Responsabilidade / Arquivos e símbolos / Funcionamento / Dependências / Acoplamento / Maturidade / Evidência de testes / Falhas e riscos / Segurança / Portabilidade para Go / Compatibilidade com Runstead / Forma de aproveitamento / Recomendação / Prioridade / Esforço / Dependências prévias.

### 24.1 Core runtime and persistence

| Campo | Snapshot+journal session persistence | Agent turn loop | Storage primitives | Crash recovery |
|---|---|---|---|---|
| **Componente do JCode** | `Session` + JSONL journal | `Agent::run_turn` (turn_loops.rs) | `jcode-storage` | `session/crash.rs` |
| **Responsabilidade** | Durable session transcript + replay | Drive model/tool loop to completion | Atomic/durable file writes, secret hygiene | Detect crashed sessions, resume |
| **Arquivos e símbolos** | `jcode-base/src/session.rs:93`; `session/persistence.rs:175-241,317`; `session/journal.rs:73` | `jcode-app-core/src/agent/turn_loops.rs:31`; `turn_execution.rs:6`; `streaming.rs` | `crates/jcode-storage/src/lib.rs:486-685` | `crates/jcode-base/src/session/crash.rs:14-23,204` |
| **Funcionamento** | Full snapshot JSON + append-only JSONL entries; replay skips/salvages corrupt lines; checkpoint deletes journal | Loop: build context -> provider stream -> tool execution -> append results -> repeat until no tools | Temp file + rename (+fsync durable / +chmod secret / .bak hard link); one O_APPEND write per journal line | PID files + session status; timestamp-based crash grouping; resume lookup |
| **Dependências** | serde_json, chrono, storage crate | tokio, provider-core, tool-core, compaction, memory | std only | std + storage |
| **Acoplamento** | Médio (session <- agent/server/TUI) | Alto (everything) | Baixo | Médio |
| **Maturidade** | Estável (long-lived) | Estável, multiple variants | Estável | Estável |
| **Evidência de testes** | T: replay/salvage/backup tests read in persistence.rs/storage | T: many turn_loops/agent tests exist | T: env injection, windows hardening tests | T: crash.rs test module |
| **Falhas e riscos** | Crash between effect and save loses effect record; JSON files grow; no transactions | Sequential tool execution; completion unverified; hidden coupling to cache/memory | No fsync on fast path (power loss) | PID reuse risk mitigated by timestamp groups |
| **Segurança** | Secrets written owner-only | n/a | Secret hardening real | n/a |
| **Portabilidade para Go** | Fácil (concepts; SQLite supersedes files) | Moderada (concepts) | Fácil (os.Rename, fsync) | Fácil (PID files) |
| **Compatibilidade com Runstead** | Alta (M2 SQLite design aligns) | Média (protocol/verifier differ) | Alta (principles) | Alta (resume/inspect) |
| **Forma de aproveitamento** | Conceito + invariantes de teste | Conceito | Algoritmo (independente) | Conceito |
| **Recomendação** | ADAPTAR EM BREVE | ADAPTAR EM BREVE | ADOTAR AGORA (invariantes) | ADAPTAR EM BREVE |
| **Prioridade** | 5 | 4 | 5 | 4 |
| **Esforço** | M | L | S | M |
| **Dependências prévias** | SQLite store (M2), event schema | Provider real (protected), loop issue #7 | Nenhuma | SQLite + checkpoints |

### 24.2 Safety and tools

| Campo | Command-risk gate | Tool contract/registry | Workspace/path safety | Action protocol/parser |
|---|---|---|---|---|
| **Componente do JCode** | `jcode-command-risk` | `Tool` trait + `ToolContext` + registry | (ausente no core; per-tool) | (não existe; native tool calls) |
| **Responsabilidade** | Classify shell blast radius; reflect/deny | Uniform tool execution with schema/intent | Jail paths to workspace | Structured action contract |
| **Arquivos e símbolos** | `crates/jcode-command-risk/src/lib.rs:44,190`; `gate.rs:78`; `tokenize.rs`; `paths.rs` | `crates/jcode-tool-core/src/lib.rs:102-166`; `jcode-app-core/src/tool/mod.rs:657-679` | Runstead: `internal/tools/workspace.go:44-104` (JCode: nenhum) | Runstead: `internal/protocol/parser.go:91-178` (JCode: `turn_loops.rs:308-390` native) |
| **Funcionamento** | Tokenize; unwrap wrappers; classify Safe/Low/Confirm/Catastrophic; gate with justification | `execute(input, ctx) -> ToolOutput`; intent injected into schemas; context guard for large outputs | EvalSymlinks + Rel boundary; reject `..`, absolute, symlink escape | Strict envelope extraction + strict JSON + typed failures |
| **Dependências** | std only | anyhow, async-trait, message/tool types | std (both) | std |
| **Acoplamento** | Baixo | Médio | Baixo | Baixo |
| **Maturidade** | Novo (issue #604 aftermath) | Estável | (Runstead) estável | (Runstead) estável |
| **Evidência de testes** | T: assess_tests, gate_tests, paths_tests, tokenize_tests | T: tool-core intent tests; registry tests | T: Runstead workspace_test.go | T: Runstead parser_test.go |
| **Falhas e riscos** | Statically undecidable shells; justification-unlockable Confirm | Errors untyped (anyhow); no repeat guard; no permission-before-effect | (Runstead) none found | (Runstead) none found |
| **Segurança** | Defense-in-depth, not sandbox | Gate is opt-in | Strong structural boundary | Strong structural boundary |
| **Portabilidade para Go** | Fácil (portable algorithm) | Fácil (interface) | n/a (Runstead owns) | n/a (Runstead owns) |
| **Compatibilidade com Runstead** | Alta (as pre-check in shell policy) | Média (typed failures needed) | n/a | n/a |
| **Forma de aproveitamento** | Algoritmo (implementação independente) | Conceito (intent + context guard) | n/a | n/a |
| **Recomendação** | ADOTAR AGORA (apenas classificação; sem justificação model) | ADAPTAR EM BREVE | n/a | n/a |
| **Prioridade** | 4 | 3 | n/a | n/a |
| **Esforço** | M | S | n/a | n/a |
| **Dependências prévias** | Política de shell aprovada (após loop read-only) | Loop read-only | n/a | n/a |

### 24.3 Providers

| Campo | Provider trait + EventStream | Provider runtimes | Error taxonomy | Routing/catalog/pricing |
|---|---|---|---|---|
| **Componente do JCode** | `Provider` trait | `jcode-provider-*-runtime` | `Error` types + classifiers | `RouteSelection`, `ModelRoute`, pricing |
| **Responsabilidade** | Uniform streaming completion | Concrete adapters | Classify failures | Model/route selection, cost estimates |
| **Arquivos e símbolos** | `crates/jcode-provider-core/src/lib.rs:76-487` | `crates/jcode-provider-openai-runtime/src/` etc. | `transport.rs`, `failover.rs`, `retry_after.rs` | `lib.rs:675-1248`; `models.rs`; `pricing.rs` |
| **Funcionamento** | `complete(messages, tools, system, resume_session_id) -> EventStream` | SDK/HTTP per provider, stream normalization | Typed transport/timeout/auth/rate errors; transient detection | Tables + live catalogs + route keys |
| **Dependências** | tokio, reqwest, message types | provider-core + SDKs | provider-core | serde + catalogs |
| **Acoplamento** | Médio | Médio | Baixo | Médio |
| **Maturidade** | Estável | Parcial-estável | Estável | Estável |
| **Evidência de testes** | T: provider-core tests | T: runtime tests + matrix tests | T: classifier tests | T: models tests, matrix tests |
| **Falhas e riscos** | Wide trait (57 methods) invites router drift | SDK bloat; auth complexity | Error strings leak | Catalog staleness |
| **Segurança** | Credential modes explicit | Redaction patterns | n/a | n/a |
| **Portabilidade para Go** | Moderada (concepts only) | Difícil (SDKs) | Fácil (concepts) | Difícil (tables) |
| **Compatibilidade com Runstead** | Média (one-call seam already) | Baixa (OmniRoute only) | Alta (omniroute already similar) | Baixa (non-goal) |
| **Forma de aproveitamento** | Conceito (streaming events p/ futuro) | Nenhuma | Conceito | Nenhuma |
| **Recomendação** | ADIAR | REJEITAR (por ora) | ADAPTAR EM BREVE | REJEITAR |
| **Prioridade** | 2 | 0 | 3 | 0 |
| **Esforço** | M | XL | S | XL |
| **Dependências prévias** | Loop protegido | Bake-off pós-M6 | Nenhuma | N/A |

### 24.4 Context, memory, UI, API, extras

| Campo | Compaction core | Memory/embeddings | TUI stack | Harness API/SDK | Swarm/ambient/overnight |
|---|---|---|---|---|---|
| **Componente do JCode** | `jcode-compaction-core` + manager | `jcode-memory-types`, `jcode-embedding`, `memory*.rs` | `jcode-tui*` (9 crates) | `jcode-harness-api(-server)`, `jcode-sdk` | `jcode-swarm-core`, `jcode-ambient-types`, `jcode-overnight-core` |
| **Responsabilidade** | Keep context in budget, never silent loss | Vector memory recall/extraction | Terminal UI, side panels, mermaid | External client API + SDK | Multi-agent + unattended autonomy |
| **Arquivos e símbolos** | `crates/jcode-compaction-core/src/lib.rs:238-291,362-387,449-692` | `crates/jcode-memory-types/src/lib.rs`; `jcode-base/src/memory*.rs` | `crates/jcode-tui/src/` (201k lines) | `crates/jcode-harness-api/src/lib.rs:1-100` | `crates/jcode-swarm-core/src/lib.rs`; `jcode-app-core/src/ambient*` |
| **Funcionamento** | Token budgets + safe cutoffs + emergency markers + provider accounting | ONNX embeddings; graph; extraction sideagent; trailing injection | ratatui rendering, custom scrollback, mermaid, panels | NDJSON frames over Unix socket, versioned | Server-owned coordination; schedulers; safety review queue |
| **Dependências** | message types | tract, tokenizers (~87 MB model) | ratatui, crossterm, wgpu (desktop) | tokio, serde | plan, task types |
| **Acoplamento** | Baixo | Médio | Alto | Médio | Alto |
| **Maturidade** | Estável | Parcial | Estável | Parcial | Parcial |
| **Evidência de testes** | T: extensive inline tests | T: memory tests | T: many TUI tests | T: schema snapshot/capability | T: swarm tests |
| **Falhas e riscos** | Model summaries lose requirements | Staleness, injection, RAM | Flicker/perf complexity | Premature public API | Coordination complexity |
| **Segurança** | n/a | Injection risk (untrusted content) | n/a | Socket auth | Review queue Design-only |
| **Portabilidade para Go** | Fácil (concepts) | Difícil (model runtime) | Difícil | Fácil (protocol idea) | Difícil |
| **Compatibilidade com Runstead** | Média (milestone C) | Baixa (non-goal) | Baixa (non-goal) | Baixa (inspect/resume CLI basta) | Muito baixa (non-goal) |
| **Forma de aproveitamento** | Algoritmo + testes | Conceito (sem vetores) | Conceito (trace shape) | Conceito (versionamento) | Nenhuma |
| **Recomendação** | ADIAR | REJEITAR (por ora) | REJEITAR (por ora) | REJEITAR (por ora) | REJEITAR |
| **Prioridade** | 3 | 0 | 0 | 1 | 0 |
| **Esforço** | M | XL | XL | M | XL |
| **Dependências prévias** | Loop + token usage | N/A | N/A | Estado durável + loop | N/A |

---

## 25. Transplantable idea versus transplantable code

| # | Recommendation | Classification |
|---|---|---|
| 1 | Journal/snapshot durability invariants (torn-line salvage, checkpoint-after-corruption) | **Copiar apenas o conceito**; reimplement against SQLite WAL/transactions in Go |
| 2 | Blast-radius command classification (tokenizer, wrapper unwrap, redirect detection) | **Portar o algoritmo com implementação independente** (Go); reject the justification gate |
| 3 | Epoch-based interrupt (`reset_if_epoch`) | **Portar o algoritmo** (~30 lines; Go: atomic counter + context; tests copied as *design*, written fresh) |
| 4 | Provider error taxonomy / retry-after semantics | **Inspirar uma interface**; Runstead's `omniroute/errors.go` already implements a narrower equivalent |
| 5 | Tool `intent` field ("why this call is being made") | **Inspirar uma interface**; becomes a mandatory prompt/protocol contract field, not a schema injection |
| 6 | Explicit truncation + `untrusted` markers | **Reutilizar o desenho de testes**; Runstead already implements the concept better (`docs/tools.md`) |
| 7 | Safe compaction cutoff (tool-call/result pairing) | **Copiar apenas o conceito**; implement in Go when compaction lands |
| 8 | Emergency markers instead of silent drops | **Copiar apenas o conceito**; implement in Go |
| 9 | Provider accounting normalization (split vs subset cache tokens) | **Copiar apenas o conceito**; document as heuristic; no code needed now |
| 10 | Race-hammer cancellation tests, corrupt-replay tests, schema snapshots | **Reutilizar o desenho de testes** (write Go tests with the same invariants, not the same code) |
| 11 | Budget-gated CI scripts | **Copiar apenas o conceito** (Go: small `scripts/check_*` or Makefile targets) |
| 12 | Anything from the TUI, desktop, swarm, memory, SDK, server | **Não reutilizar** |

**Do not reuse JCode code directly**, even though the license is MIT. Reasons: (a) the report's mandate is concept-level adoption; (b) Rust-to-Go translation of these algorithms is small and should be written against Runstead's types and invariants; (c) the interesting parts (tests, invariants) are language-independent and should be re-expressed in Go; (d) if any algorithm is ever ported closely, attribution is required by the MIT license (preserve the copyright notice). No code was copied in this analysis.

---

## 26. Adoption recommendations (summary)

- **ADOTAR AGORA** (does not require new upstream capabilities):
  1. Durability invariants for the future SQLite event store (concept + test design). Prerequisite: none (can be written as a design doc + test skeleton now; implementation lands with M2).
  2. Epoch-based cancel semantics + race-hammer tests in Go (`context` wrapper). Prerequisite: none; `cmd/runstead` already handles signals.
  3. CI budget gates and error-handling discipline (concept). Prerequisite: none.
  4. Blast-radius classification algorithm (Go, independent implementation). Prerequisite: only as part of a future shell policy boundary; can be prototyped as a leaf package with tests without wiring it into tools.
- **ADAPTAR EM BREVE** (after the read-only loop is stable): action-fingerprint/repeat-guard refinements (already exist), typed tool failure taxonomy alignment with observations, tool status lifecycle in trace output, parser correction policy informed by JCode's response-recovery ideas (bounded counters), streaming event vocabulary when streaming arrives.
- **ADIAR** (milestones C): compaction (pair-preserving cutoff, markers, budgets), resume/checkpoint reconstruction details, provider accounting heuristic, harness API versioning discipline, token usage accounting.
- **REJEITAR**: server daemon, multi-client, TUI, desktop, swarm, ambient/overnight, vector memory, MCP, provider routing/catalogs/pricing, native tool-calling contract, justification-unlockable gates, public SDK now.

---

## 27. Phased integration plan for Runstead

### A. Adotar agora (no new upstream capability required)

| Candidate | Runstead problem it solves | Adaptation to Go | Package | Interface | Persisted data | Invariants | Tests | Risks | Acceptance | Prereqs |
|---|---|---|---|---|---|---|---|---|---|---|
| **A1. Durability/event-store invariants doc + test skeleton** | M2 SQLite store must not lose or duplicate events | Concept: append-only events, WAL transactions, checkpoint-after-corruption, forensic copy | `internal/state` (future) | `Store.Append(event)`, `Store.Replay(from)` | events, checkpoints | every effect has exactly one event; replay is total | torn-write, crash-between-effect-and-commit, duplicate-append tests | over-design; keep SQLite as a file, not a service | replay after simulated crash contains all committed events, no duplicates | none (design + tests only) |
| **A2. Epoch-based cancel + race tests** | `runstead run` must never lose Ctrl+C and must never double-execute after cancel | `cancel.Signal` = atomic flag + epoch + channel; `ResetIfEpoch` | `internal/agent` | `Signal` type | none | a newer cancel is never erased by a stale reset; fired cancel is always observed | 2000-iteration race hammer in Go | goroutine leaks | cancel observed exactly once, no lost wakeups | none |
| **A3. CI budget gates** | prevent regression bloat | small shell/Go checks (file size, test size, `go vet` clean) | CI (`ci.yml`) | n/a | none | budgets fail loudly | CI green | false positives on legit growth | budgets documented and bumpable via PR | none |

### B. Adaptar após o loop read-only estável

| Candidate | Problem | Adaptation | Package | Interface | Persisted | Invariants | Tests | Risks | Acceptance | Prereqs |
|---|---|---|---|---|---|---|---|---|---|---|
| **B1. Blast-radius classifier (leaf, unused until shell policy)** | future shell tool needs deterministic pre-check | Go tokenizer + wrapper unwrap + redirects; levels Safe/Low/Confirm/Catastrophic; **no model justification**; Confirm -> human approval | `internal/risk` | `Assess(cmd, ctx) Assessment` | none | Catastrophic absolute deny; parser total (never panics) | corpus tests from JCode's cases + new shell cases | static parser defeat (`sh -c`), scope creep | corpus pass; deny always fails closed | loop read-only + approval UX decision |
| **B2. Typed tool failure taxonomy + status lifecycle in trace** | observations must carry typed failures; trace must show lifecycle | extend `internal/tools` `Failure` codes; emit running/completed/error lines with id+duration | `internal/tools`, `internal/trace` | existing | events already | one terminal status per observation | status transition tests | churn in trace format | `runstead` trace shows lifecycle for every action | loop #7 |
| **B3. Correction-policy counters (bounded)** | parser must not loop forever on malformed output | JCode's bounded counters (context-limit, incomplete, empty-post-tool) as Go loop budgets | `internal/agent` | loop config | loop state | budgets are ceilings, not defaults to exceed | budget exhaustion tests | over-engineering | loop terminates with typed outcome | loop #7 |

### C. Reavaliar em milestones posteriores

- Compaction: pair-preserving cutoff, explicit markers, budget thresholds, provider accounting heuristic (needs token usage from provider + bounded context reconstruction).
- Resume/checkpoint: bounded context reconstruction from SQLite; `runstead resume`; session id as disposable metadata (already doctrine).
- Harness API: only the versioning discipline (major/minor, unknown-field tolerance) as a pattern for protocol evolution; no public API.
- Streaming: event vocabulary (TextDelta, ToolUse, TokenUsage, SessionId, RetryRollback semantics) when OmniRoute streaming is adopted.
- Memory (non-vector): durable working-summary entity with source/timestamp/trust, trailing marked injection.

### D. Rejeitar (com justificativa)

- Server daemon/sockets/hot reload, TUI, desktop, swarm, ambient/overnight, vector memory, MCP, provider routing/catalog/pricing, public SDK, native tool-calling as contract, justification gates, `jcode-command-risk`'s `Confirm` unlock, embedding runtime, session picker, side panels, mermaid, notifications/email.

---

## 28. Proposed issue backlog

Ordered to respect current gates (governor, protected provider, read-only loop). Do **not** create these as GitHub issues without review.

### RS-01 — Document event-store durability invariants (inspired by JCode snapshot/journal)

- **Título:** Define durability and replay invariants for the SQLite event store
- **Objetivo:** Freeze the invariants the M2 store must satisfy before implementation.
- **Contexto:** JCode's snapshot+journal design (salvage of torn lines, checkpoint-after-corruption, no silent truncation) is the closest working precedent; SQLite WAL changes the mechanics but not the invariants.
- **Escopo:** Design doc + acceptance test skeleton (Go) simulating torn/duplicate/crash scenarios against a real SQLite file; document durable-vs-fast write tiers.
- **Fora de escopo:** implementing the store, migrations, `inspect`/`resume`.
- **Desenho técnico:** events = immutable rows with monotonic ids; derived state = updatable; checkpoint = periodic compaction of the event log with forensic copy; WAL + transactions for atomicity.
- **Arquivos afetados:** `docs/research/` (this report), `docs/architecture.md`, future `internal/state`.
- **Invariantes:** replay is total (no committed event lost, no duplicate); a crash between effect and commit leaves the effect uncommitted and detectable; corruption is isolated, never truncates the tail.
- **Testes:** torn-write, mid-commit kill, duplicate append, corrupt page, checkpoint-then-crash.
- **Critérios de aceitação:** all skeleton tests pass against a real SQLite file; document records which invariants mirror JCode (`persistence.rs:26-129,151-173`).
- **Dependências:** none (can start immediately).
- **Origem:** JCode `jcode-base/src/session/persistence.rs`, `crates/jcode-storage/src/lib.rs:486-685`.
- **Riscos de sobre-engenharia:** do not build an event-sourcing framework; SQLite is the store, not a bus.

### RS-02 — Epoch-based cancellation signal with race tests

- **Título:** Add an epoch-based cancel signal for the agent loop
- **Objetivo:** Guarantee Ctrl+C is never lost and a stale reset never erases a newer cancel.
- **Contexto:** JCode's `InterruptSignal` (flag + tokio Notify + fire epoch) exists precisely because lost cancels caused real bugs (issue #428); Go's `context` needs the same discipline around `runstead run`.
- **Escopo:** small `internal/agent` (or `internal/cancel`) type + race-hammer tests; wire into the signal-aware entrypoint.
- **Fora de escopo:** the agent loop itself.
- **Desenho técnico:** `Signal{flag atomic.Bool, epoch atomic.Uint64, ch chan struct{}}`; `Fire` bumps epoch; `ResetIfEpoch` restores a racing fire.
- **Invariantes:** fire never lost; concurrent fire never erased; double-fire idempotent.
- **Testes:** 2000-iteration race hammer (mirror `fire_never_loses_wakeup`), reset races.
- **Critérios:** race tests pass under `-race`; no goroutine leaks.
- **Dependências:** none.
- **Origem:** JCode `crates/jcode-agent-runtime/src/lib.rs:33-118,180-282`.
- **Risco:** over-abstracting; keep it one small type, no framework.

### RS-03 — Blast-radius shell risk classifier (leaf, not wired)

- **Título:** Prototype a deterministic blast-radius shell classifier
- **Objetivo:** Have the algorithm ready and tested before any shell tool is designed.
- **Contexto:** JCode's `command-risk` shows the right approach (classify by blast radius, unwrap wrappers, detect redirects/pipes) and the wrong one (model justification unlock). Runstead must keep human-only approval.
- **Escopo:** `internal/risk` with `Assess`; corpus of safe/low/confirm/catastrophic cases; no wiring into tools, no approval UX.
- **Fora de escopo:** shell tool, approval flow, policy config.
- **Desenho técnico:** tokenizer (quotes, flags, operators), wrapper unwrap (sudo/env/xargs/timeout), shell recursion (`sh -c`), redirect targets, catastrophic path deny (home/root/credentials).
- **Invariantes:** parser total; Catastrophic absolute; unknown -> escalate.
- **Testes:** corpus from JCode's test cases + new Go cases.
- **Critérios:** corpus pass; `go vet`/`-race` clean.
- **Dependências:** none (RS-01/RS-02 independent).
- **Origem:** JCode `crates/jcode-command-risk/src/lib.rs:146-361` (algorithm only).
- **Risco:** scope creep into a shell tool; keep it a leaf package.

### RS-04 — Typed tool failure taxonomy and trace lifecycle

- **Título:** Align tool failure taxonomy with trace lifecycle output
- **Objetivo:** Every observation has one typed terminal status and the trace shows Running->Completed/Error with id and duration.
- **Contexto:** Runstead already has typed `Failure` codes (`internal/tools`); JCode's `ToolEvent` status lifecycle is the presentation pattern.
- **Escopo:** extend failure codes if gaps found; emit lifecycle lines in `internal/trace`.
- **Fora de escopo:** changing observation JSON schema.
- **Desenho:** status transitions enum; trace writer emits start/end lines.
- **Invariantes:** one terminal status per observation.
- **Testes:** transition tests; trace golden tests.
- **Critérios:** trace of a multi-action run shows lifecycle; no schema break.
- **Dependências:** loop #7 (to observe real runs).
- **Origem:** JCode `ToolStatus` (Running/Completed/Error), `turn_loops.rs:928-1098`.
- **Risco:** trace format churn; keep additive.

### RS-05 — Parser correction policy with bounded counters

- **Título:** Define bounded correction and continuation budgets for the agent loop
- **Objetivo:** The loop must terminate with a typed outcome under malformed/empty/truncated responses.
- **Contexto:** JCode's `run_turn` uses bounded counters (context-limit retries, incomplete continuations, empty-post-tool) and stop-reason classification; Runstead's parser is strict but the loop is #7 work.
- **Escopo:** design + budget constants + unit tests for the counter logic; wire in with #7.
- **Fora de escopo:** provider retries (governor-owned).
- **Desenho:** loop budgets as config; typed terminal outcomes (`loop_exhausted`, `canceled`, `completed`).
- **Invariantes:** budgets are ceilings; exhaustion is a typed outcome, not an error string.
- **Testes:** budget exhaustion, empty response, truncation, repeated actions.
- **Critérios:** loop terminates; repeated actions rejected before execution.
- **Dependências:** #7 loop; RS-02.
- **Origem:** JCode `turn_loops.rs:44-52,832-860`, `response_recovery.rs:56-120` (concept only).
- **Risco:** over-engineering corrections; keep one-envelope-per-turn.

### RS-06 — CI quality gates

- **Título:** Add CI budget and error-handling gates
- **Objetivo:** Fail loudly on bloat and swallowed errors.
- **Contexto:** JCode runs `check_*_budget.py` gates; Runstead's CI currently runs `go test` only.
- **Escopo:** small scripts + CI steps (size budgets per package, `go vet`, errcheck-style discipline).
- **Fora de escopo:** coverage mandates.
- **Desenho:** budgets in a checked-in JSON; bump via PR.
- **Testes:** CI self-check.
- **Critérios:** CI fails on budget breach.
- **Dependências:** none.
- **Origem:** JCode `scripts/check_code_size_budget.py`, `check_swallowed_error_budget.py`, `check_test_size_budget.py`.
- **Risco:** brittle budgets; keep thresholds coarse.

### RS-07 — Compaction design note (deferred)

- **Título:** Record compaction design constraints for the deferred milestone
- **Objetivo:** Preserve JCode-informed decisions (pair-preserving cutoff, explicit markers, provider accounting heuristic) in one doc.
- **Escopo:** doc only (`docs/research/` + architecture note).
- **Fora de escopo:** implementation.
- **Critérios:** doc exists and is referenced by the roadmap's deferred list.
- **Dependências:** none.
- **Origem:** JCode `crates/jcode-compaction-core/src/lib.rs`.
- **Risco:** premature design; keep it a note.

### RS-08 — Event-store schema snapshot tests (part of M2 prep)

- **Título:** Freeze protocol and event JSON shapes with snapshot tests
- **Objetivo:** Prevent accidental drift of `runstead.protocol.v1` envelopes and event rows.
- **Contexto:** JCode freezes harness API frames with schema snapshot tests.
- **Escopo:** golden JSON fixtures for action/final envelopes and future event rows; tests fail on drift.
- **Fora de escopo:** new protocol features.
- **Critérios:** golden files committed; drift fails CI.
- **Dependências:** none.
- **Origem:** JCode `crates/jcode-harness-api/src/lib.rs:35-47` (concept).
- **Risco:** churn when protocol legitimately evolves; version the golden files.

---

## 29. First issue that is actually worth executing

**RS-02 — Epoch-based cancellation signal with race tests.**

Rationale:
- It is the smallest, highest-leverage, dependency-free step. `cmd/runstead` already owns signal-aware startup (`cmd/runstead/main.go:26`), so the type slots in immediately and is testable in isolation with `go test -race`.
- It unblocks the real loop issue (#7): the loop's cancel semantics are the foundation for "no lost cancel, no double execution", which Runstead's durability goals depend on. Every later issue (RS-04, RS-05) consumes it.
- It directly imports JCode's best-tested invariant (the `fire_never_loses_wakeup` race hammer), which is exactly the kind of reliability engineering Runstead prioritizes, without pulling any JCode architecture.
- Everything else must wait: RS-01 is design-only (valuable but not executable code), RS-03 must not be wired before a policy boundary exists, RS-04/RS-05 require the loop (#7) that is explicitly deferred, and the M2 items require SQLite decisions.

Concrete capability unlocked: a `runstead run` loop (when #7 lands) that can be canceled safely at any point, resumes from SQLite checkpoints without double-executing the last effect, and has a typed, testable cancellation primitive shared by the loop, the governor (cancellation before `Start` releases the permit) and future tools.

---

## 30. Explicitly rejected components

| Component | Why rejected |
|---|---|
| Single-server multi-client daemon + sockets + hot reload | Conflicts with local-first, disposable-remote, single binary |
| TUI stack (ratatui, side panels, mermaid, pickers, anim) | Non-goal; only trace-shape ideas adopted |
| Desktop app (`jcode-desktop2`, wgpu/Vello) | Non-goal |
| Vector memory + local embeddings (~87 MB model, sideagents) | Non-goal; RAM, staleness, injection risk |
| Swarm / ambient / overnight / notifications / email | Non-goal; complexity without current benefit |
| Provider routing, catalogs, pricing, fallback chains, auto model selection | Would make Runstead a generic router; violates non-goals |
| MCP support | Deferred explicitly |
| Public harness API / SDK now | Premature; `inspect`/`resume` CLI is the interface |
| Native tool-calling as the contract | Violates protocol independence |
| Justification-unlockable destructive gate | Delegates authorization to the model; violates fail-closed |
| Tool `intent` schema injection (as-is) | Adopted only as a protocol/prompt concept, not a schema hack |
| jemalloc/glibc allocator tuning, opt-level pinning | Rust-specific; no demonstrated Go need |
| Session picker, account picker, server naming | Cosmetic/UI; non-goal |
| `jcode-command-risk`'s `Confirm` tier | Model-unlockable; replaced by human approval in Runstead |

---

## 31. Legal, technical and maintenance risks

### 31.1 License

- JCode root is **MIT** (`LICENSE`, Copyright (c) 2025 Jeremy Huang). C.
- Subcomponents to verify before any reuse decision (not fully audited):
  - `assets/` (images/videos in the repo; release-hosted media), `scripts/`, `telemetry-worker/`, `ios/`, `sdk/` may carry their own notices. Not audited line-by-line. NV.
  - Bundled crates are third-party (tokio, reqwest, ratatui, tract, etc.) with their own licenses; not relevant unless code is copied.
- **Consequences for Runstead:** MIT permits copying with attribution, but this report recommends *not copying code* at all. If a future PR ports an algorithm closely (e.g. the blast-radius tokenizer), it must preserve the JCode copyright notice in the new file and record provenance in the PR. Recommended header: `// Ported concept from jcode (MIT, Copyright (c) 2025 Jeremy Huang): <algorithm>. Reimplemented independently.`
- No assets, models or binaries from JCode were copied or will be copied.

### 31.2 Technical risks of adopting JCode ideas

1. **Invariant translation loss**: JCode's journal invariants assume JSON files; SQLite WAL changes failure modes (page corruption, WAL truncation). The invariants must be re-derived for SQLite, not assumed.
2. **Race-hammer flakiness**: Go race tests need `-race`; 2000-iteration hammers can be slow in CI. Bound iterations and use a configurable count.
3. **Blast-radius classifier scope**: if wired prematurely into a shell tool, it creates a false sense of security (JCode itself documents "defense in depth, not a sandbox"). Must remain a pre-check inside an explicit policy boundary with human approval.
4. **Compaction deferral**: adopting compaction before token accounting/context reconstruction exists would create silent requirement loss. It stays deferred.
5. **Protocol hardening creep**: JCode's response-recovery heuristics are tuned to specific providers; Runstead's strict parser should remain strict and *not* adopt lenient recovery except through the bounded correction channel.

### 31.3 Maintenance risks

- Porting "concepts" without documenting provenance produces future confusion about what came from JCode. Every adopted idea should carry a reference to this report's section.
- The Runstead monolith must not grow JCode-style megafiles; budget gates (RS-06) protect against that.

---

## 32. Commands executed and results

### 32.1 Runstead (executed)

| Command | Result |
|---|---|
| `git rev-parse HEAD` | `56d0aa9c5ff79bf68dd1735fd01442668e7a97a4` |
| `git branch --show-current` | `RenyEnnos/ladyfish` |
| `git status --short` | (empty, clean) |
| `git log --oneline` | 17 commits, latest "Add a bounded read-only tool registry (#6)" |
| `git ls-files \| wc -l` | 102 |
| `go test ./...` | all packages `ok` (cmd/runstead, agent, config, governor, protocol, provider, omniroute, tools, trace) |
| `go version` | go1.22.2 linux/amd64 |
| GitHub API issues/repo | 404 Not Found (private repo) |

### 32.2 JCode (static only)

| Command | Result |
|---|---|
| `git clone https://github.com/1jehuang/jcode.git /tmp/jcode-re` | OK (26s) |
| `git rev-parse HEAD` | `435fb4a83bee429762acd1cc905ba9987bff65d7` |
| `git log -1 --format=fuller` | 2026-08-05 21:00:35 -0700, "chore(release): v0.70.1" |
| `git rev-list --count HEAD` | 6810 |
| `git ls-files \| wc -l` | 1890 |
| `git ls-files '*.rs' \| wc -l` | 1237 |
| `git tag` | latest v0.70.1 |
| `cargo metadata / cargo check / cargo test` | **NOT EXECUTED** — no Rust toolchain installed on this machine (`command -v cargo` empty). See section 33. |

### 32.3 What the dynamic verification would have added

- Confirmation that leaf crates compile (`cargo check -p jcode-command-risk -p jcode-compaction-core -p jcode-tool-core -p jcode-provider-core -p jcode-storage -p jcode-agent-runtime`).
- Execution of the small-crate test suites (command-risk, compaction-core, agent-runtime) to confirm the race hammers and corpus tests pass.
- `cargo metadata --format-version 1` for the exact dependency graph.

These remain **Não foi possível verificar** and are the only part of this report not backed by execution.

---

## 33. Gaps and unverified points

1. **All JCode tests are read, not executed** (no Rust toolchain). Line-level test behavior is reported as "T (test code read)".
2. **Runstead issues/PRs**: private; reconstructed from git history. Issues #29/#30 (attempt receipts) status is doc-declared, not confirmed by any code that consumes receipts (the adapter returns `ErrUnsafeRoute` regardless).
3. **JCode README benchmarks not reproduced**; methodology exists but no machine/toolchain to run them.
4. **Deep reads skipped** for the very large files (TUI 201k lines, desktop2 43k lines, app-core 134k lines): structural inventory only.
5. **License subcomponent audit** of JCode incomplete (assets, SDK, scripts, telemetry-worker not line-audited).
6. **JCode version skew**: README benchmark tables reference `v0.9.1888-dev`; analyzed commit is v0.70.1. Numbers in this report are only cited as claims.
7. **Runstead LICENSE file**: none exists yet; legal review of Runstead itself is out of scope here but recommended before release.
8. **`jcode-tui` re-export chain**: confirmed as C after reading `crates/jcode-tui/src/lib.rs:14-33` (`pub use jcode_app_core::*`, which re-exports `jcode-base`); no longer an inference.

---

## 34. Evidence index (key files)

### JCode (all paths relative to the analyzed commit)

| Evidence | Path |
|---|---|
| Workspace members, profiles, features | `Cargo.toml:8-93,208-235,285-770` |
| Composition root | `src/main.rs:105-140`, `src/lib.rs:22-31` |
| Modular architecture RFC (Draft) | `docs/MODULAR_ARCHITECTURE_RFC.md:3,31-36,144-156,220-292` |
| Crate ownership boundaries | `docs/CRATE_OWNERSHIP_BOUNDARIES.md` |
| Server architecture | `docs/SERVER_ARCHITECTURE.md:14-33,53-126` |
| Safety system (Design) | `docs/SAFETY_SYSTEM.md` |
| Browser provider protocol (draft) | `docs/BROWSER_PROVIDER_PROTOCOL.md:1-30` |
| Agent turn loop | `crates/jcode-app-core/src/agent/turn_loops.rs:31-1130` |
| Turn execution / context | `crates/jcode-app-core/src/agent/turn_execution.rs:6-35,344-399` |
| Response recovery | `crates/jcode-app-core/src/agent/response_recovery.rs:56-120` |
| Interrupt signal + tests | `crates/jcode-agent-runtime/src/lib.rs:33-118,180-282` |
| Tool contract | `crates/jcode-tool-core/src/lib.rs:12-26,44-166` |
| Registry context guard / pre_tool | `crates/jcode-app-core/src/tool/mod.rs:657-679,733-848` |
| Bash destructive gate | `crates/jcode-app-core/src/tool/bash_destructive_gate.rs:8-33` |
| Command risk | `crates/jcode-command-risk/src/lib.rs:1-361`, `gate.rs:78-122`, `paths.rs`, `tokenize.rs` |
| Provider trait / routing | `crates/jcode-provider-core/src/lib.rs:71-72,76-487,645-666,675-1248` |
| Storage primitives | `crates/jcode-storage/src/lib.rs:225-432,486-685,748-769` |
| Session persistence | `crates/jcode-base/src/session/persistence.rs:26-129,131-173,175-241` |
| Crash recovery | `crates/jcode-base/src/session/crash.rs:14-23,204` |
| Session model | `crates/jcode-base/src/session.rs:93-123` |
| Compaction core | `crates/jcode-compaction-core/src/lib.rs:5-75,77-85,238-291,362-387,449-692,722-1035` |
| Compaction manager | `crates/jcode-base/src/compaction.rs:134,514,561` |
| Memory types | `crates/jcode-memory-types/src/lib.rs:10-417` |
| Harness API | `crates/jcode-harness-api/src/lib.rs:1-100,35-47` |
| Swarm core | `crates/jcode-swarm-core/src/lib.rs:1-50` |
| Benchmark methodology | `scripts/bench_startup.py:1-80`, `scripts/memory_regression_gate.sh:1-50`, `scripts/memory_probe.sh` |
| Budget gates | `scripts/check_code_size_budget.py`, `check_swallowed_error_budget.py`, `check_test_size_budget.py`, `check_dependency_boundaries.py` |
| License | `LICENSE` |

### Runstead (all paths relative to commit `56d0aa9`)

| Evidence | Path |
|---|---|
| Product direction, phases, non-goals | `README.md:1-158` |
| Architecture, loop, policy, state | `docs/architecture.md:1-320` |
| Roadmap and milestones | `docs/roadmap.md:1-213` |
| Governor SLO, single-attempt, circuits | `docs/account-protection.md:1-186` |
| Tool registry contracts | `docs/tools.md:1-106` |
| Development environment | `docs/development.md:1-236` |
| Protocol decision (M0) | `experiments/protocol/DECISION.md` |
| CLI entrypoint | `cmd/runstead/main.go:25-142` |
| Parser + repeat guard | `internal/protocol/parser.go:91-178,423-488` |
| Provider seam | `internal/provider/provider.go:12-105` |
| OmniRoute fail-closed client | `internal/provider/omniroute/client.go:85-213` |
| Governor | `internal/governor/governor.go:36-107` |
| Executor seam | `internal/agent/executor.go:13-37` |
| Workspace jail | `internal/tools/workspace.go:21-104` |
| Tool registry | `internal/tools/registry.go:69-204`, `execute.go:10-82` |
| Tests | 131 functions across `*_test.go`; `go test ./...` green |

---

## 35. Conclusion

**Quais partes do JCode realmente tornam o Runstead melhor sem destruir a simplicidade, a segurança e a direção arquitetural do Runstead?**

1. **A disciplina de durabilidade e recuperação** do snapshot+journal do JCode (salvage de linhas corrompidas, checkpoint após corrupção, distinção entre escrita durável e rápida, cópia forense) traduzida em invariantes para o SQLite do M2. Isto fortalece exatamente os pilares do Runstead: recuperação, evidência e previsibilidade.
2. **A classificação determinística de risco por blast radius** (tokenização, desembrulho de wrappers, redirecionamentos e pipes), como *pré-checagem* dentro de uma futura política de shell com aprovação humana — nunca com desbloqueio por justificativa do modelo.
3. **O sinal de cancelamento com época** (`reset_if_epoch`) e seus testes de corrida, portado como um pequeno tipo Go com `go test -race`, garantindo que Ctrl+C nunca se perca e que um reset atrasado nunca apague um cancelamento mais novo.
4. **O desenho de testes de confiabilidade** (race hammers, replay de entradas corrompidas, snapshots de schema, budgets de CI, matrizes sem provider real) — o padrão de engenharia mais portável e barato de todos.
5. **Os marcadores explícitos em vez de perda silenciosa** (truncamento com contagens, substituição de conteúdo por marcadores) — já implementados no modelo de observação do Runstead e confirmados como o padrão correto pela prática do JCode.

**O que deve ser rejeitado:** toda a camada de autoridade do JCode sobre efeitos (tool calling nativo como contrato, gates por ferramenta, desbloqueio por justificativa, ausência de verificação de conclusão), a arquitetura de servidor multi-cliente, memória vetorial, TUI/desktop, swarm/ambient, roteamento de providers e a API pública prematura.

O Runstead não deve se tornar um "JCode em Go". Ele deve permanecer um monólito modular em Go, com o protocolo `runstead.protocol.v1` estrito, o governor fail-closed como única fronteira de autorização e o SQLite como fonte autoritativa — enriquecido apenas pelas ideias acima, adotadas como conceitos e algoritmos reimplementados de forma independente, com proveniência documentada.

---

*End of report. Analyzed JCode `435fb4a83bee429762acd1cc905ba9987bff65d7` (v0.70.1, 2026-08-05) and Runstead `56d0aa9c5ff79bf68dd1735fd01442668e7a97a4` (2026-08-03). No functional changes were made to either repository; no code was copied; no commit or PR was created.*
