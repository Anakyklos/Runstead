#!/usr/bin/env python3
"""
run_spike.py - disposable live-proof orchestrator for the Runstead
first-party ChatGPT Web research spike (issue #16).

Drives the ORCA EMBEDDED BROWSER exclusively through the `orca` CLI
(browser automation section). It never uses the generic jcode browser
backend (firefox_agent_bridge) and never touches production Runstead code.

Live model turn budget: 2 total.
  1. one bounded success turn (RUNSTEAD_FIRST_PARTY_OK)
  2. one cancellation-after-dispatch turn (only if dispatch was observed)
Plus: cancellation-before-dispatch (no model-effect request), and
fail-closed drift/failure fixtures (no model calls).

All recorded artifacts are sanitized: no headers, no cookies, no tokens,
no Authorization material, no prompt/response body beyond the bounded
spike tokens and conversation ids needed for correlation.
"""

import argparse
import json
import os
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
OUT = os.path.join(HERE, "output")

PROMPT_OK = "Reply with exactly: RUNSTEAD_FIRST_PARTY_OK"
PROMPT_CANCEL = "Reply with exactly: RUNSTEAD_CANCELLED_OK"

DEFAULT_TAB = "f6d5b169-fac5-4efc-bf70-cbd3ae94e65b"

lifecycle = []


def log_event(kind, **fields):
    entry = {"kind": kind, "ts": time.time(), **fields}
    lifecycle.append(entry)
    print(f"[{kind}] {json.dumps(fields, ensure_ascii=False)[:400]}", flush=True)
    return entry


def run(cmd, timeout=90, check=False):
    """Run a command, return (rc, stdout, stderr)."""
    try:
        proc = subprocess.run(
            cmd,
            capture_output=True,
            text=True,
            timeout=timeout,
        )
        return proc.returncode, proc.stdout, proc.stderr
    except subprocess.TimeoutExpired as e:
        return 124, "", f"timeout after {timeout}s: {e}"


def orca(args, timeout=90):
    return run(["orca"] + args, timeout=timeout)


def eval_js(page, expression, timeout=60):
    """Evaluate JS in the given Orca browser page; returns parsed JSON."""
    rc, out, err = orca(
        ["eval", "--page", page, "--expression", expression], timeout=timeout
    )
    if rc != 0:
        raise RuntimeError(f"orca eval failed rc={rc} stderr={err.strip()[:300]}")
    out = out.strip()
    if not out:
        return None
    try:
        return json.loads(out)
    except json.JSONDecodeError:
        return {"raw": out}


def read_script(name):
    with open(os.path.join(HERE, name), "r", encoding="utf-8") as f:
        return f.read()


def fnv1a_utf16(text):
    """UTF-16 code-unit length + FNV-1a 32-bit hash, matching the JS side."""
    h = 2166136261
    length = 0
    for ch in text:
        for code in [ord(ch)]:  # BMP-only prompts in this spike
            h = ((h ^ code) * 16777619) & 0xFFFFFFFF
            length += 1
    return length, h


def install_scripts(page):
    instrument = read_script("instrument.js")
    harness = read_script("harness.js")
    r1 = eval_js(page, instrument)
    r2 = eval_js(page, harness)
    if not (r1 and r1.get("installed")) or not (r2 and r2.get("harness") == "installed"):
        raise RuntimeError(f"script install failed: {r1} / {r2}")
    return r1, r2


def ready_check(page, tag):
    state = eval_js(page, "window.__rsHarness.ready()")
    log_event("ready_check", tag=tag, **state)
    return state


def fingerprint_check(page, expected_text):
    fp = eval_js(page, "window.__rsHarness.composerFingerprint()")
    exp_len, exp_hash = fnv1a_utf16(expected_text)
    ok = bool(fp.get("present")) and fp.get("length") == exp_len and fp.get("hash") == exp_hash
    log_event(
        "fingerprint",
        present=bool(fp.get("present")),
        expected_len=exp_len,
        actual_len=fp.get("length"),
        expected_hash=exp_hash,
        actual_hash=fp.get("hash"),
        match=ok,
    )
    return ok, fp


def composer_ref(page):
    """Find the composer textbox a11y ref from a fresh Orca snapshot."""
    rc, out, err = orca(["snapshot", "--page", page], timeout=60)
    if rc != 0:
        raise RuntimeError(f"orca snapshot failed rc={rc} stderr={err.strip()[:200]}")
    import re
    m = re.search(
        r'textbox "(Converse com o ChatGPT|Chat with ChatGPT)" \[ref=([a-z0-9]+)\]',
        out,
    )
    if not m:
        raise RuntimeError("composer textbox not found in snapshot (DOM drift)")
    return m.group(2)


def fill_composer(page, prompt):
    """Fill the composer through the Orca accessibility path (setValue)."""
    ref = composer_ref(page)
    rc, out, err = orca(["fill", "--page", page, "--element", ref, "--value", prompt], timeout=90)
    if rc != 0:
        raise RuntimeError(f"orca fill failed rc={rc} stderr={err.strip()[:200]}")
    log_event("filled", element_ref=ref, length=len(prompt))
    return ref


def set_phase(page, phase):
    eval_js(page, f'window.__rsPhase = "{phase}"; true')


def request_summary(page):
    return eval_js(page, "window.__rsSpike.summary()")


def reset_log(page):
    eval_js(page, "window.__rsSpike.reset()")


def classify_entries(entries):
    """Split observed requests into model-effect vs auxiliary."""
    model_effect = []
    aux = []
    for e in entries:
        is_model = (
            e.get("method") == "POST"
            and "chatgpt.com" in (e.get("hostname") or "")
            and "conversation" in (e.get("path") or "")
        )
        (model_effect if is_model else aux).append(e)
    return model_effect, aux


def turn_success(page):
    """Phase: one bounded text turn with full observability."""
    log_event("turn", turn="success", prompt_hash=fnv1a_utf16(PROMPT_OK), budget="1/2")

    reset_log(page)
    set_phase(page, "armed")

    state = ready_check(page, "success-preflight")
    if state.get("verdict") != "ready":
        log_event("turn_aborted", turn="success", reason=f"verdict={state.get('verdict')}")
        return {"status": "aborted"}

    fill_composer(page, PROMPT_OK)
    time.sleep(0.8)

    ok, fp = fingerprint_check(page, PROMPT_OK)
    if not ok:
        log_event("turn_aborted", turn="success", reason="fingerprint mismatch")
        eval_js(page, "window.__rsHarness.clearComposer()")
        return {"status": "aborted", "reason": "fingerprint mismatch"}

    set_phase(page, "submitted")
    click = eval_js(page, "window.__rsHarness.clickSend()")
    log_event("dispatched", turn="success", clicked=click.get("clicked"))
    if not click.get("clicked"):
        set_phase(page, "cancelled")
        log_event("turn_aborted", turn="success", reason="send button missing/disabled")
        return {"status": "aborted", "reason": "send not clicked"}

    # Poll DOM for final-response signals.
    started = time.time()
    timeout = 240
    seen_busy = False
    seen_text = False
    terminal_state = None
    while time.time() - started < timeout:
        st = eval_js(page, "window.__rsHarness.turnState()")
        if st.get("busy") and not seen_busy:
            seen_busy = True
            log_event("response_started", turn="success", signal="stop_button")
        if st.get("textLength", 0) > 0 and not seen_text:
            seen_text = True
            log_event("response_streaming", turn="success", signal="assistant_text")
        if st.get("terminal"):
            terminal_state = st
            break
        time.sleep(1.5)

    if terminal_state is None:
        log_event("turn_timeout", turn="success", elapsed=time.time() - started)
        # Fail closed: click stop if still generating; no retry.
        eval_js(page, "window.__rsHarness.clickStop()")
        return {
            "status": "uncertain",
            "reason": "no terminal signal within timeout",
            "busy_seen": seen_busy,
            "text_seen": seen_text,
        }

    text = terminal_state.get("exactText", "")
    expected = "RUNSTEAD_FIRST_PARTY_OK"
    text_ok = text.strip() == expected
    log_event(
        "final_response",
        turn="success",
        text_match=text_ok,
        text_length=len(text.strip()),
        conversation_id=terminal_state.get("conversationId"),
        signal="terminal_markers",
    )

    set_phase(page, "done")
    time.sleep(2)  # let the fetch wrapper finish reading the stream
    summary = request_summary(page)
    model_effect, aux = classify_entries(summary.get("entries", []))
    log_event(
        "accounting",
        turn="success",
        logical_turns=1,
        physical_model_effect_sends=len(model_effect),
        total_observed_requests=summary.get("total"),
        conversations_seen=summary.get("conversations"),
    )
    for e in model_effect:
        log_event(
            "model_effect_request",
            turn="success",
            seq=e.get("seq"),
            method=e.get("method"),
            hostname=e.get("hostname"),
            path=e.get("path"),
            status=e.get("status"),
            phase=e.get("phase"),
            dispatched_at=e.get("dispatchedAt"),
            completed_at=e.get("ts"),
            conversation_id=e.get("conversationId"),
        )
    for e in aux:
        log_event(
            "aux_request",
            turn="success",
            seq=e.get("seq"),
            method=e.get("method"),
            hostname=e.get("hostname"),
            path=e.get("path"),
            status=e.get("status"),
            phase=e.get("phase"),
        )

    conv_url = terminal_state.get("conversationId")
    conv_net = summary.get("conversations") or []
    correlated = bool(conv_url) and conv_url in conv_net
    log_event("correlation", turn="success", url_conversation_id=conv_url, network_ids=conv_net, match=correlated)

    return {
        "status": "ok" if text_ok else "mismatch",
        "physical_sends": len(model_effect),
        "correlated": correlated,
        "conversation_id": conv_url,
    }


def turn_cancel_pre_dispatch(page):
    """Phase: cancellation before dispatch must produce zero model-effect sends."""
    log_event("turn", turn="cancel-pre", budget="no model turn")

    reset_log(page)
    set_phase(page, "armed")
    state = ready_check(page, "cancel-pre-preflight")
    if state.get("verdict") != "ready":
        log_event("turn_aborted", turn="cancel-pre", reason=f"verdict={state.get('verdict')}")
        return {"status": "aborted"}

    fill_composer(page, PROMPT_OK)
    time.sleep(0.8)
    ok, _ = fingerprint_check(page, PROMPT_OK)
    if not ok:
        log_event("turn_aborted", turn="cancel-pre", reason="fingerprint mismatch")
        eval_js(page, "window.__rsHarness.clearComposer()")
        return {"status": "aborted", "reason": "fingerprint mismatch"}

    # Cancel decision lands BEFORE dispatch: clear the composer, never submit.
    set_phase(page, "cancelled")
    cleared = eval_js(page, "window.__rsHarness.clearComposer()")
    log_event("cancelled_before_dispatch", turn="cancel-pre", cleared=cleared.get("cleared"))

    summary = request_summary(page)
    model_effect, aux = classify_entries(summary.get("entries", []))
    log_event(
        "accounting",
        turn="cancel-pre",
        logical_turns=0,
        physical_model_effect_sends=len(model_effect),
        total_observed_requests=summary.get("total"),
    )
    for e in aux:
        log_event("aux_request", turn="cancel-pre", seq=e.get("seq"), method=e.get("method"),
                  hostname=e.get("hostname"), path=e.get("path"), status=e.get("status"))
    for e in model_effect:
        log_event("model_effect_request", turn="cancel-pre", seq=e.get("seq"), method=e.get("method"),
                  hostname=e.get("hostname"), path=e.get("path"), status=e.get("status"))

    return {"status": "ok" if len(model_effect) == 0 else "unexpected_send", "physical_sends": len(model_effect)}


def turn_cancel_post_dispatch(page):
    """Phase: cancellation after dispatch (second and final live model turn)."""
    log_event("turn", turn="cancel-post", budget="2/2")

    reset_log(page)
    set_phase(page, "armed")
    state = ready_check(page, "cancel-post-preflight")
    if state.get("verdict") != "ready":
        log_event("turn_aborted", turn="cancel-post", reason=f"verdict={state.get('verdict')}")
        return {"status": "aborted"}

    fill_composer(page, PROMPT_CANCEL)
    time.sleep(0.8)
    ok, _ = fingerprint_check(page, PROMPT_CANCEL)
    if not ok:
        log_event("turn_aborted", turn="cancel-post", reason="fingerprint mismatch")
        eval_js(page, "window.__rsHarness.clearComposer()")
        return {"status": "aborted", "reason": "fingerprint mismatch"}

    set_phase(page, "submitted")
    click = eval_js(page, "window.__rsHarness.clickSend()")
    log_event("dispatched", turn="cancel-post", clicked=click.get("clicked"))
    if not click.get("clicked"):
        set_phase(page, "cancelled")
        log_event("turn_aborted", turn="cancel-post", reason="send button missing/disabled")
        return {"status": "aborted", "reason": "send not clicked"}

    # Wait for evidence of dispatch: stop button appears or assistant text starts.
    started = time.time()
    dispatched_evidence = None
    while time.time() - started < 60:
        st = eval_js(page, "window.__rsHarness.turnState()")
        if st.get("busy") or st.get("textLength", 0) > 0:
            dispatched_evidence = {
                "busy": st.get("busy"),
                "text_length": st.get("textLength"),
            }
            break
        time.sleep(1.0)

    if dispatched_evidence is None:
        log_event("cancel_post_no_dispatch_evidence", turn="cancel-post")
        set_phase(page, "cancelled")
        return {"status": "uncertain", "reason": "no dispatch evidence before stop"}

    log_event("dispatch_confirmed", turn="cancel-post", **dispatched_evidence)

    # Now cancel via the page's own stop mechanism. No retry afterwards.
    stopped = eval_js(page, "window.__rsHarness.clickStop()")
    log_event("stop_clicked", turn="cancel-post", clicked=stopped.get("clicked"))

    # Wait for generation to actually stop.
    stop_wait_started = time.time()
    final_state = None
    while time.time() - stop_wait_started < 45:
        st = eval_js(page, "window.__rsHarness.turnState()")
        if not st.get("busy"):
            final_state = st
            break
        time.sleep(1.0)
    if final_state is None:
        final_state = eval_js(page, "window.__rsHarness.turnState()")
        log_event("cancel_post_stop_timeout", turn="cancel-post")

    final_text = final_state.get("exactText", "") if final_state else ""
    log_event(
        "cancelled_after_dispatch",
        turn="cancel-post",
        final_text_length=len(final_text.strip()),
        final_text_hash=final_state.get("textHash") if final_state else None,
        busy=final_state.get("busy") if final_state else None,
        terminal=final_state.get("terminal") if final_state else None,
    )

    set_phase(page, "done")
    time.sleep(2)
    summary = request_summary(page)
    model_effect, aux = classify_entries(summary.get("entries", []))
    log_event(
        "accounting",
        turn="cancel-post",
        logical_turns=1,
        physical_model_effect_sends=len(model_effect),
        total_observed_requests=summary.get("total"),
    )
    for e in model_effect:
        log_event(
            "model_effect_request",
            turn="cancel-post",
            seq=e.get("seq"),
            method=e.get("method"),
            hostname=e.get("hostname"),
            path=e.get("path"),
            status=e.get("status"),
            phase=e.get("phase"),
            dispatched_at=e.get("dispatchedAt"),
            completed_at=e.get("ts"),
            conversation_id=e.get("conversationId"),
        )

    # Classification: delivered / response_started / uncertain.
    text_len = len(final_text.strip())
    if len(model_effect) >= 1:
        if text_len > 0:
            state_class = "response_started"
        else:
            state_class = "delivered"
    else:
        state_class = "uncertain"
    log_event("cancel_post_classification", turn="cancel-post", state=state_class, physical_sends=len(model_effect))
    return {"status": state_class, "physical_sends": len(model_effect)}


def parse_tab_id(out):
    """orca tab create prints 'Created tab <uuid>'; extract the id."""
    import re
    m = re.search(r"([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})", out or "")
    return m.group(1) if m else None


def fixtures(page):
    """Phase: failure/drift classification, all pre-dispatch, no model calls."""
    # a) Session/profile unavailable: bogus page id must fail closed.
    rc, out, err = orca(["eval", "--page", "00000000-0000-0000-0000-000000000000", "--expression", "1"], timeout=30)
    log_event("failure_fixture", fixture="session_unavailable", rc=rc, error=err.strip()[:160])

    # b) Contract unknown: about:blank tab.
    rc, out, err = orca(["tab", "create", "--url", "about:blank"], timeout=45)
    blank_id = parse_tab_id(out)
    if blank_id:
        install_scripts(blank_id)
        st = ready_check(blank_id, "about-blank")
        log_event("failure_fixture", fixture="contract_missing", verdict=st.get("verdict"),
                  url=st.get("url"), acted=False)
        orca(["tab", "close", "--page", blank_id], timeout=30)

    # c) Login-marker fixture at a non-ChatGPT origin.
    fixture_path = os.path.join(HERE, "fixtures", "logged-out.html")
    os.makedirs(os.path.dirname(fixture_path), exist_ok=True)
    with open(fixture_path, "w", encoding="utf-8") as f:
        f.write(
            "<!doctype html><html><body>"
            "<button>Log in</button><button>Sign up</button>"
            "<div>Fake login interstitial for spike fixture</div>"
            "</body></html>"
        )
    rc, out, err = orca(["tab", "create", "--url", "file://" + fixture_path], timeout=45)
    fix_id = parse_tab_id(out)
    if fix_id:
        install_scripts(fix_id)
        st = ready_check(fix_id, "login-marker-fixture")
        log_event("failure_fixture", fixture="login_expired_or_wrong_origin", verdict=st.get("verdict"),
                  signed_out_markers=st.get("signedOut"), url=st.get("url"), acted=False)
        orca(["tab", "close", "--page", fix_id], timeout=30)


def classification_matrix(page):
    """Pure-function test of the fail-closed verdict matrix (no page mutation)."""
    probes = [
        {"name": "nominal", "onChatGPT": True, "composer": True, "signedOut": False, "dialogPresent": False, "expect": "ready"},
        {"name": "wrong-origin", "onChatGPT": False, "composer": True, "signedOut": False, "dialogPresent": False, "expect": "contract_missing"},
        {"name": "login-expired", "onChatGPT": True, "composer": True, "signedOut": True, "dialogPresent": False, "expect": "login_required"},
        {"name": "unknown-composer", "onChatGPT": True, "composer": False, "signedOut": False, "dialogPresent": False, "expect": "contract_missing"},
        {"name": "dialog-blocking", "onChatGPT": True, "composer": True, "signedOut": False, "dialogPresent": True, "expect": "dialog_blocking"},
    ]
    expr = (
        "JSON.stringify("
        + json.dumps(probes)
        + '.map(p => ({name: p.name, verdict: window.__rsHarness.classify(p), expected: p.expect, pass: window.__rsHarness.classify(p) === p.expect})))'
    )
    res = eval_js(page, expr)
    for r in (res or []):
        log_event("classify_matrix", **r)
    return res


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--tab", default=DEFAULT_TAB)
    ap.add_argument("--skip-live", action="store_true", help="skip live model turns (fixtures + matrix only)")
    args = ap.parse_args()

    os.makedirs(OUT, exist_ok=True)
    log_event("start", tab=args.tab, prompt=PROMPT_OK)

    rc, out, err = orca(["status"], timeout=30)
    log_event("orca_status", rc=rc)
    if rc != 0:
        log_event("fatal", reason="orca runtime unreachable", error=err.strip()[:200])
        sys.exit(2)

    install_scripts(args.tab)
    classification_matrix(args.tab)

    if args.skip_live:
        fixtures(args.tab)
    else:
        success = turn_success(args.tab)
        cancel_pre = turn_cancel_pre_dispatch(args.tab)
        if success.get("status") == "ok" and success.get("physical_sends", 0) >= 1:
            # Second and final live turn: cancellation after dispatch.
            cancel_post = turn_cancel_post_dispatch(args.tab)
        else:
            cancel_post = {"status": "skipped", "reason": "success turn did not confirm dispatch"}
            log_event("cancel_post_skipped", reason=cancel_post["reason"])
        fixtures(args.tab)

    summary = {
        "success_turn": success if not args.skip_live else {"status": "skipped"},
        "cancel_pre": cancel_pre if not args.skip_live else {"status": "skipped"},
        "cancel_post": cancel_post if not args.skip_live else {"status": "skipped"},
    }
    log_event("end", **summary)

    with open(os.path.join(OUT, "lifecycle.json"), "w", encoding="utf-8") as f:
        json.dump(lifecycle, f, indent=2, ensure_ascii=False)
    with open(os.path.join(OUT, "summary.json"), "w", encoding="utf-8") as f:
        json.dump(summary, f, indent=2, ensure_ascii=False)
    print(f"\nArtifacts written to {OUT}/")


if __name__ == "__main__":
    main()
