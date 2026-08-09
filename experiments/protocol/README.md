# Runstead protocol experiment

This is the disposable M0 harness for issue #2. It tests a Runstead-owned text
protocol through an OpenAI-compatible, non-streaming OmniRoute chat endpoint.
The simulated tools read only the committed fixture workspace; they never run
arbitrary host commands or modify files.

## Candidate protocol

Version: `runstead.protocol.v1`.

Every model response must contain exactly one envelope. Short prose outside a
single valid envelope is recorded as `mixed_prose` but does not execute.
Multiple or unclosed envelopes are rejected.

```xml
<runstead_action>
{"version":"runstead.protocol.v1","tool":"read_file","arguments":{"path":"README.md"}}
</runstead_action>
```

```xml
<runstead_final>
{"version":"runstead.protocol.v1","status":"complete","summary":"...","evidence":[{"evidence_id":"obs-000001","tool":"read_file"}]}
</runstead_final>
```

The action schema uses exactly `version`, `tool` and `arguments`. The final
schema uses exactly `version`, `status`, `summary` and `evidence`. Every
evidence entry is a typed citation: the model declares the tool that produced
the cited observation, and the runtime verifier rejects a fabricated, foreign
or type-incompatible citation (issue #11). Only
`read_file` and `list_files` are enabled. Unknown tools, malformed JSON,
protocol refusals and unsupported execution claims are classified without
execution. A normalized `tool + arguments` fingerprint identifies repeated
actions; repeats are not executed again.

The correction budget is two attempts per session. The runner sends a
structured correction observation and stops the session when the budget is
exhausted. Defaults are three independent sessions and five successful
read-only tool turns per session.

## Host-native run

Requirements: Bash, `jq`, and `curl` for live mode.

Run the deterministic parser and replay proof first:

```sh
bash experiments/protocol/test.sh
experiments/protocol/run.sh --offline
```

For OmniRoute, inject credentials only in the process environment. Do not use
shell tracing and do not put the key in a fixture, Dockerfile, or command
argument.

```sh
export OMNIROUTE_BASE_URL=http://localhost:20128/v1
export OMNIROUTE_API_KEY='injected-at-runtime'
export OMNIROUTE_MODEL='the-ChatGPT-Web-model-or-combo-configured-in-OmniRoute'
experiments/protocol/run.sh --live
```

`OMNIROUTE_CHAT_ENDPOINT` can override the default
`${OMNIROUTE_BASE_URL%/}/chat/completions` URL. The runner sets `stream:false`,
uses a fresh `X-Session-Id` for each independent session, and disables the
OmniRoute cache for the experiment.

## Docker run

The image contains only Bash, `curl`, `jq` and certificates. The API key is
passed at runtime and is never copied into an image layer.

```sh
docker build -f experiments/protocol/Dockerfile -t runstead-protocol .
docker run --rm --network host \
  -e OMNIROUTE_BASE_URL=http://localhost:20128/v1 \
  -e OMNIROUTE_API_KEY \
  -e OMNIROUTE_MODEL \
  runstead-protocol --live
```

On Docker Desktop, use the host gateway address supported by that platform
instead of `--network host`.

## Evidence and replay

Each run writes to an ignored `results/<run-id>/` directory unless `--output`
is supplied. It contains sanitized request/response captures, parser outcomes,
structured observations, transport classifications, `events.jsonl`,
`report.json` and `report.md`. Unsanitized provider bodies and temporary curl
configuration are removed after each turn.

`fixtures/corpus/` is transport-neutral response material for parser replay.
`fixtures/sessions/` provides three independent offline transcripts, including
one mixed-prose action, one malformed action and one repeated action. Live
captures retain the provider envelope separately from the extracted model text
so transport failures cannot be mistaken for protocol failures.

The report measures parse outcomes, correction attempts/successes, refusals,
unsupported claims, repeated actions and separate transport/provider/protocol
failure buckets. The offline report deliberately remains `REVISE`; only a live
run with the default three sessions and five successful tool turns can provide
the OmniRoute evidence needed to adopt the protocol for M1.
