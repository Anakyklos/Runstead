"""Synthetic, offline tests for the Stage 2 failure evidence checker."""
import hashlib
import importlib.util
import contextlib
import io
import json
import os
import sqlite3
import subprocess
import sys
import tempfile
import unittest
from unittest import mock
from pathlib import Path


RUNNER = Path(__file__).with_name("stage2-runner.py")
SPEC = importlib.util.spec_from_file_location("stage2_runner_for_tests", RUNNER)
RUNNER_MODULE = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = RUNNER_MODULE
SPEC.loader.exec_module(RUNNER_MODULE)
TASK_ID = "cli-1791085826705830653"
TEST_KEY = "unit-test-key-never-a-real-credential"
OBJECTIVE = (
    "Read app/calc.go and identify the whitespace-handling bug in ParseValues. "
    "Do not modify files and do not run recipes/processes. "
    "Base the final claim on the actual read_file evidence."
)
SOURCE_HASH = "b8a1bd5dc67bfd9a64bd13503994fdf5e3fc78edaf80502f29992561c6986d94"
PROVIDER = "groq-gpt-oss-120b-canary-v2"
MODEL = "openai/gpt-oss-120b"
FAMILY = "openai_compatible"
CONFIG_IDENTITY = RUNNER_MODULE.EXPECTED_CONFIG_IDENTITY


def digest(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def make_state(state_dir: Path) -> None:
    state_dir.mkdir(parents=True, exist_ok=True)
    db_path = state_dir / "runstead.db"
    if db_path.exists():
        db_path.unlink()
    db = sqlite3.connect(db_path)
    db.executescript("""
        CREATE TABLE tasks (
            task_id TEXT PRIMARY KEY, objective TEXT, status TEXT, outcome TEXT,
            resume_count INTEGER, model TEXT, config_json TEXT,
            execution_contract_json TEXT, execution_contract_hash TEXT
        );
        CREATE TABLE acceptance_plans (
            task_id TEXT PRIMARY KEY, version INTEGER, spec_json TEXT, digest TEXT, created_at TEXT
        );
        CREATE TABLE provider_attempts (
            execution_id TEXT PRIMARY KEY, task_id TEXT, client_request_id TEXT,
            provider TEXT, protocol_family TEXT, config_identity TEXT, model TEXT,
            attempt_sequence INTEGER, status TEXT, outcome TEXT, delivery_state TEXT,
            request_id TEXT, upstream_reached INTEGER, uncertain INTEGER, attempt_debited INTEGER
        );
        CREATE TABLE events (task_id TEXT, sequence INTEGER, kind TEXT, payload_json TEXT);
        CREATE TABLE actions (action_id TEXT, task_id TEXT);
        CREATE TABLE tool_attempts (execution_id TEXT, task_id TEXT);
        CREATE TABLE tool_results (evidence_id TEXT, task_id TEXT);
        CREATE TABLE verification_attempts (attempt_id TEXT, task_id TEXT);
        CREATE TABLE governor_task_states (task_id TEXT, attempts INTEGER, retries INTEGER);
        CREATE TABLE governor_ledger (id INTEGER PRIMARY KEY, task_id TEXT);
        CREATE TABLE write_policy_decisions (id INTEGER PRIMARY KEY, task_id TEXT);
        CREATE TABLE approvals (approval_id TEXT, task_id TEXT);
    """)
    check = {
        "id": "calc-source-unchanged", "type": "file_hash",
        "path": "app/calc.go", "sha256": SOURCE_HASH,
    }
    plan = {"version": 1, "checks": [check]}
    check_json = json.dumps(check, ensure_ascii=False, separators=(",", ":"))
    plan_digest = digest(f"version=1\ncalc-source-unchanged={check_json}".encode())
    provider = {
        "provider_id": PROVIDER,
        "protocol_family": FAMILY,
        "model": MODEL,
        "config_identity": CONFIG_IDENTITY,
        "provider_profile_version": "v1",
        "adapter_version": "compatible-provider-v0.1",
    }
    tools_digest = "sha256:" + digest(json.dumps(
        RUNNER_MODULE.EXPECTED_TOOLS, ensure_ascii=False, separators=(",", ":")
    ).encode())
    contract = {
        "contract_version": 1,
        "runtime_identity": "runstead-runtime.v1",
        "protocol_identity": "runstead.protocol.v1",
        "profile": {"id": "groq-canary-v2-read-only", "version": "1.0.0"},
        "packages": [RUNNER_MODULE.EXPECTED_PACKAGE],
        "provider": provider,
        "tools": RUNNER_MODULE.EXPECTED_TOOLS,
        "tool_schema_digest": tools_digest,
        "recipe_catalog": {},
        "acceptance_plan_digest": plan_digest,
        "governor_identity": "runstead.governor.v1",
        "policy_identity": "runstead.policy.v1",
        "evidence_identity": "runstead.evidence.v1",
        "recovery_identity": "runstead.recovery.v1",
        "verifier_identity": "runstead.verifier.v1",
    }
    contract_json = RUNNER_MODULE.canonical_contract_bytes(contract).decode("utf-8")
    task_config = {
        "provider_id": PROVIDER,
        "protocol_family": FAMILY,
        "provider_model": MODEL,
        "model": MODEL,
        "provider_config_identity": CONFIG_IDENTITY,
        "provider_profile_version": "v1",
        "provider_adapter_version": "compatible-provider-v0.1",
        "acceptance_plan_digest": plan_digest,
    }
    request_id = TASK_ID + "-0001"
    execution_id = "exec-000001"
    db.execute(
        "INSERT INTO tasks VALUES (?, ?, 'failed', 'final_not_grounded', 0, ?, ?, ?, ?)",
        (TASK_ID, OBJECTIVE, MODEL, json.dumps(task_config, separators=(",", ":")),
         contract_json, "sha256:" + digest(contract_json.encode())),
    )
    db.execute(
        "INSERT INTO acceptance_plans VALUES (?, 1, ?, ?, '2026-01-01T00:00:00Z')",
        (TASK_ID, json.dumps(plan, separators=(",", ":")), plan_digest),
    )
    db.execute(
        "INSERT INTO provider_attempts VALUES (?, ?, ?, ?, ?, ?, ?, 1, 'completed', 'success', 'completed', ?, 1, 0, 1)",
        (execution_id, TASK_ID, request_id, PROVIDER, FAMILY, CONFIG_IDENTITY, MODEL,
         "sha256:0123456789abcdef"),
    )
    events = [
        (1, "acceptance_plan_saved", {"digest": plan_digest}),
        (2, "provider_attempt_prepared", {
            "execution_id": execution_id, "client_request_id": request_id,
            "provider": PROVIDER, "model": MODEL, "protocol_family": FAMILY,
            "config_identity": CONFIG_IDENTITY, "governor": {"admitted": True},
        }),
        (3, "provider_attempt_completed", {
            "client_request_id": request_id, "status": "completed",
            "upstream_reached": True, "uncertain": False,
            "attempt_debited": 1, "config_identity": CONFIG_IDENTITY,
            "outcome": "success", "delivery_state": "completed",
            "request_id": "sha256:0123456789abcdef",
        }),
        (4, "task_finalized", {"status": "failed", "outcome": "final_not_grounded"}),
    ]
    db.executemany(
        "INSERT INTO events VALUES (?, ?, ?, ?)",
        [(TASK_ID, sequence, kind, json.dumps(payload, separators=(",", ":")))
         for sequence, kind, payload in events],
    )
    db.execute("INSERT INTO governor_task_states VALUES (?, 1, 0)", (TASK_ID,))
    db.execute("INSERT INTO governor_ledger (task_id) VALUES (?)", (TASK_ID,))
    db.commit()
    db.close()


class Stage2ExistingAuditTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.state_dir = Path(self.tmp.name) / "state"
        make_state(self.state_dir)

    def tearDown(self):
        self.tmp.cleanup()

    def run_audit(self, key=TEST_KEY, task_id=TASK_ID):
        env = os.environ.copy()
        env.pop("GROQ_API_KEY", None)
        if key is not None:
            env["GROQ_API_KEY"] = key
        return subprocess.run(
            [sys.executable, str(RUNNER), "--audit-existing",
             "--runstead-bin", "/path/that/must-not-be-invoked",
             "--state-dir", str(self.state_dir), "--task-id", task_id],
            env=env, text=True, capture_output=True, check=False,
        )

    def mutate(self, sql, params=()):
        db = sqlite3.connect(self.state_dir / "runstead.db")
        db.execute(sql, params)
        db.commit()
        db.close()

    def test_expected_failure_shape_is_confirmed_without_claiming_gate_pass(self):
        before_hash = digest((self.state_dir / "runstead.db").read_bytes())
        result = self.run_audit()
        after_hash = digest((self.state_dir / "runstead.db").read_bytes())
        self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
        self.assertEqual(after_hash, before_hash, "successful audit changed the SQLite file")
        self.assertIn("audit_result=EVIDENCE_SHAPE_CONFIRMED", result.stdout)
        self.assertIn("db_predicates=pass", result.stdout)
        self.assertIn("stage2=NOT_QUALIFIED", result.stdout)
        self.assertIn("gate_a=NOT_SATISFIED", result.stdout)
        self.assertIn("secret_scan=clean", result.stdout)
        self.assertIn(TASK_ID, result.stdout)
        self.assertNotIn(TEST_KEY, result.stdout + result.stderr)

    def test_attempt_count_mismatch_is_rejected(self):
        self.mutate(
            "INSERT INTO provider_attempts VALUES (?, ?, ?, ?, ?, ?, ?, 2, 'completed', 'success', 'completed', ?, 1, 0, 1)",
            ("exec-000002", TASK_ID, TASK_ID + "-0002", PROVIDER, FAMILY, CONFIG_IDENTITY, MODEL,
             "sha256:fedcba9876543210"),
        )
        result = self.run_audit()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("db_predicates=fail", result.stdout)

    def test_task_config_identity_mismatch_is_rejected(self):
        db = sqlite3.connect(self.state_dir / "runstead.db")
        row = db.execute("SELECT config_json FROM tasks WHERE task_id=?", (TASK_ID,)).fetchone()
        config = json.loads(row[0])
        config["provider_config_identity"] = "different-sanitized-config"
        db.execute("UPDATE tasks SET config_json=? WHERE task_id=?",
                   (json.dumps(config, separators=(",", ":")), TASK_ID))
        db.commit()
        db.close()
        result = self.run_audit()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("db_predicates=fail", result.stdout)

    def test_acceptance_event_after_dispatch_is_rejected(self):
        db = sqlite3.connect(self.state_dir / "runstead.db")
        plan_digest = db.execute(
            "SELECT digest FROM acceptance_plans WHERE task_id=?", (TASK_ID,),
        ).fetchone()[0]
        db.execute("INSERT INTO events VALUES (?, 5, 'acceptance_plan_saved', ?)",
                   (TASK_ID, json.dumps({"digest": plan_digest})))
        db.commit()
        db.close()
        result = self.run_audit()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("db_predicates=fail", result.stdout)

    def test_unknown_acceptance_related_event_after_dispatch_is_rejected(self):
        self.mutate(
            "INSERT INTO events VALUES (?, 5, 'acceptance_plan_replaced', ?)",
            (TASK_ID, json.dumps({"digest": "different"})),
        )
        result = self.run_audit()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("db_predicates=fail", result.stdout)

    def test_completed_event_request_id_mismatch_is_rejected(self):
        db = sqlite3.connect(self.state_dir / "runstead.db")
        row = db.execute(
            "SELECT payload_json FROM events WHERE task_id=? AND kind='provider_attempt_completed'",
            (TASK_ID,),
        ).fetchone()
        payload = json.loads(row[0])
        payload["request_id"] = "sha256:fedcba9876543210"
        db.execute(
            "UPDATE events SET payload_json=? WHERE task_id=? AND kind='provider_attempt_completed'",
            (json.dumps(payload, separators=(",", ":")), TASK_ID),
        )
        db.commit()
        db.close()
        result = self.run_audit()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("db_predicates=fail", result.stdout)

    def test_boolean_true_debit_representation_is_accepted(self):
        db = sqlite3.connect(self.state_dir / "runstead.db")
        row = db.execute(
            "SELECT payload_json FROM events WHERE task_id=? AND kind='provider_attempt_completed'",
            (TASK_ID,),
        ).fetchone()
        payload = json.loads(row[0])
        payload["attempt_debited"] = True
        db.execute(
            "UPDATE events SET payload_json=? WHERE task_id=? AND kind='provider_attempt_completed'",
            (json.dumps(payload, separators=(",", ":")), TASK_ID),
        )
        db.commit()
        db.close()
        result = self.run_audit()
        self.assertEqual(result.returncode, 0, result.stderr + result.stdout)
        self.assertIn("db_predicates=pass", result.stdout)

    def test_symlink_makes_secret_scan_incomplete(self):
        target = Path(self.tmp.name) / "outside-state-file"
        target.write_text("synthetic unrelated data", encoding="utf-8")
        (self.state_dir / "linked-state-file").symlink_to(target)
        result = self.run_audit()
        self.assertEqual(result.returncode, 3, result.stderr + result.stdout)
        self.assertIn("db_predicates=pass", result.stdout)
        self.assertIn("secret_scan=incomplete", result.stdout)
        self.assertIn("audit_result=LIMITED", result.stdout)

    def test_secret_scan_enforces_file_and_total_byte_limits(self):
        scan_dir = Path(self.tmp.name) / "bounded-scan"
        scan_dir.mkdir()
        (scan_dir / "first.bin").write_bytes(b"123456")
        (scan_dir / "second.bin").write_bytes(b"abcdef")
        with mock.patch.dict(os.environ, {"GROQ_API_KEY": TEST_KEY}), \
             mock.patch.object(RUNNER_MODULE, "MAX_SCAN_FILE_BYTES", 10), \
             mock.patch.object(RUNNER_MODULE, "MAX_SCAN_TOTAL_BYTES", 10):
            self.assertEqual(RUNNER_MODULE.scan_secret(scan_dir), "incomplete")
        with mock.patch.dict(os.environ, {"GROQ_API_KEY": TEST_KEY}), \
             mock.patch.object(RUNNER_MODULE, "MAX_SCAN_FILE_BYTES", 5):
            self.assertEqual(RUNNER_MODULE.scan_secret(scan_dir), "incomplete")

    def test_static_preflight_failure_has_bounded_sanitized_output(self):
        output = io.StringIO()
        with mock.patch.dict(os.environ, {"GROQ_API_KEY": TEST_KEY}), \
             mock.patch.object(RUNNER_MODULE, "static_preflight", side_effect=RuntimeError("private detail")), \
             contextlib.redirect_stdout(output):
            result = RUNNER_MODULE.audit_existing(self.state_dir, TASK_ID)
        self.assertEqual(result, 2)
        self.assertIn("failed_predicate=stage2_static_preflight", output.getvalue())
        self.assertNotIn("private detail", output.getvalue())

    def test_malformed_task_and_attempt_ids_are_never_echoed(self):
        hostile_task = TASK_ID + "\nforged=1"
        result = self.run_audit(task_id=hostile_task)
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn(hostile_task, result.stdout + result.stderr)

        for column, hostile in (
            ("execution_id", "exec-000001\nforged=1"),
            ("client_request_id", TASK_ID + "-0001\nforged=1"),
        ):
            with self.subTest(column=column):
                make_state(self.state_dir)
                self.mutate(f"UPDATE provider_attempts SET {column}=? WHERE task_id=?", (hostile, TASK_ID))
                result = self.run_audit()
                self.assertNotEqual(result.returncode, 0)
                self.assertNotIn(hostile, result.stdout + result.stderr)

    def test_unexpected_action_is_rejected(self):
        self.mutate("INSERT INTO actions VALUES ('action-000001', ?)", (TASK_ID,))
        result = self.run_audit()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("db_predicates=fail", result.stdout)

    def test_unexpected_verifier_is_rejected(self):
        self.mutate("INSERT INTO verification_attempts VALUES ('verif-000001', ?)", (TASK_ID,))
        result = self.run_audit()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("db_predicates=fail", result.stdout)

    def test_retry_or_debit_mismatch_is_rejected(self):
        for change in (
            ("UPDATE governor_task_states SET retries=1 WHERE task_id=?", (TASK_ID,)),
            ("UPDATE provider_attempts SET attempt_debited=0 WHERE task_id=?", (TASK_ID,)),
        ):
            with self.subTest(change=change[0]):
                make_state(self.state_dir)
                self.mutate(*change)
                result = self.run_audit()
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("db_predicates=fail", result.stdout)

    def test_secret_present_in_state_is_reported_without_value(self):
        (self.state_dir / "synthetic-note.bin").write_text(TEST_KEY, encoding="utf-8")
        result = self.run_audit()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("db_predicates=pass", result.stdout)
        self.assertIn("secret_scan=present", result.stdout)
        self.assertNotIn(TEST_KEY, result.stdout + result.stderr)

    def test_unset_secret_keeps_db_result_limited(self):
        result = self.run_audit(key=None)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("db_predicates=pass", result.stdout)
        self.assertIn("secret_scan=unavailable", result.stdout)
        self.assertIn("audit_result=LIMITED", result.stdout)

    def test_audit_existing_requires_state_dir_and_task_id(self):
        result = subprocess.run(
            [sys.executable, str(RUNNER), "--audit-existing"],
            env={**os.environ, "GROQ_API_KEY": TEST_KEY},
            text=True, capture_output=True, check=False,
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("audit_existing_requires_state_dir_and_task_id", result.stdout)
        self.assertNotIn(TEST_KEY, result.stdout + result.stderr)


if __name__ == "__main__":
    unittest.main()
