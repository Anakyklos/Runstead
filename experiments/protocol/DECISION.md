# M0 protocol decision

Status: **REVISE BEFORE M1**

The experiment adopts `runstead.protocol.v1` as the candidate contract for the
proof, but the repository does not claim that ChatGPT Web is reliable through
OmniRoute from offline fixtures alone. The candidate must be revised or
adopted after a live run with at least three independent sessions and five
successful read-only tool turns per session.

## What the offline evidence establishes

- One tagged action or final envelope can be extracted from mixed prose.
- Strict JSON and exact envelope keys reject malformed actions.
- Unknown tools, refusals and unsupported execution claims are observable
  protocol failures, not tool executions.
- A canonical tool/arguments fingerprint detects repeated actions without
  re-executing them.
- Two structured correction attempts are bounded and measured.
- Sanitized raw request/response captures and normalized parser outcomes can
  be replayed without depending on OmniRoute response details.

## Live adoption gate

Run `run.sh --live` with the configured ChatGPT Web model or combo. Adopt the
candidate for M1 only if the report shows all configured sessions completed,
at least five successful read-only tool turns in each session, no unclassified
transport/provider failures, and corrections staying within the two-attempt
budget. Otherwise keep the contract at `REVISE` and use the retained corpus to
update the prompt/parser before implementing the Go runtime.

## Known failure modes

- Provider HTTP, authentication, timeout and malformed-envelope failures are
  transport/provider evidence; they do not prove model non-compliance.
- A response without a valid envelope is protocol non-compliance and is never
  executed.
- Mixed prose is tolerated only when exactly one strict envelope is present.
- Repeated actions are a policy signal and are not executed a second time.
- A session that exhausts its correction budget is incomplete even if the
  model claims success.
