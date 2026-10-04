import hashlib
import json
import sqlite3
import tempfile
import unittest
from pathlib import Path

import stage2_audit


TASK_ID = "cli-1791122946929726868"
OBJECTIVE = (
    "Read app/calc.go using the available repository tool. Complete only after citing the actual "
    "read_file observation for app/calc.go. Do not modify files and do not run recipes or processes."
)
CONFIG_IDENTITY = (
    'provider.Config{ProviderID:"groq-gpt-oss-120b-canary-v3" '
    'ProtocolFamily:"openai_compatible" Endpoint:"https://api.groq.com/openai/v1" '
    'Model:"openai/gpt-oss-120b" AuthRequirement:"reference_required" AuthRef:true '
    'Options:[] ProfileVersion:"v1" RouteSafety:provider.RouteSafety{AttemptAccounting:0x1 '
    'SingleAttempt:0x1 InternalRetries:0x1 CooldownReplay:0x1 AccountPooling:0x1 '
    'AutomaticFallback:0x1 ComboRouting:0x1} ConfigVersion:"v1"}'
)
ACCEPTANCE_DIGEST = "3c1206c11be99bbd41f780830b899902ba3bb17c807d67081a30bfdd9e932af1"


def create_expected_state(root: Path) -> Path:
    state = root / "state"
    state.mkdir()
    db_path = state / "runstead.db"
    conn = sqlite3.connect(db_path)
    conn.executescript(
        """
        CREATE TABLE tasks(task_id TEXT, objective TEXT, status TEXT, outcome TEXT,
          stop_reason TEXT, resume_count INTEGER, config_json TEXT,
          execution_contract_json TEXT, execution_contract_hash TEXT);
        CREATE TABLE provider_attempts(execution_id TEXT, task_id TEXT, client_request_id TEXT,
          provider TEXT, model_pool TEXT, model TEXT, attempt_sequence INTEGER, status TEXT, outcome TEXT,
          upstream_reached INTEGER, uncertain INTEGER, attempt_debited INTEGER,
          selected_backoff_ns INTEGER, delivery_state TEXT, protocol_family TEXT, config_identity TEXT,
          request_id TEXT, error_class TEXT, prepared_at TEXT, completed_at TEXT);
        CREATE TABLE provider_attempt_receipts(task_id TEXT);
        CREATE TABLE governor_ledger(id INTEGER, task_id TEXT, at TEXT);
        CREATE TABLE governor_task_states(task_id TEXT, attempts INTEGER, retries INTEGER);
        CREATE TABLE events(task_id TEXT, sequence INTEGER, kind TEXT, payload_json TEXT, created_at TEXT);
        CREATE TABLE acceptance_plans(task_id TEXT, version INTEGER, spec_json TEXT,
          digest TEXT, created_at TEXT);
        CREATE TABLE actions(task_id TEXT, tool TEXT);
        CREATE TABLE tool_attempts(task_id TEXT, tool TEXT);
        CREATE TABLE tool_results(task_id TEXT);
        CREATE TABLE verification_attempts(task_id TEXT);
        """
    )
    config = {
        "provider_id": "groq-gpt-oss-120b-canary-v3",
        "protocol_family": "openai_compatible",
        "provider_model": "openai/gpt-oss-120b",
        "model": "openai/gpt-oss-120b",
        "provider_config_identity": CONFIG_IDENTITY,
        "provider_profile_version": "v1",
        "provider_adapter_version": "compatible-provider-v0.1",
        "acceptance_plan_digest": ACCEPTANCE_DIGEST,
    }
    contract = {
        "contract_version": 1,
        "acceptance_plan_digest": ACCEPTANCE_DIGEST,
        "provider": {
            "provider_id": config["provider_id"],
            "protocol_family": config["protocol_family"],
            "model": config["provider_model"],
            "config_identity": CONFIG_IDENTITY,
            "provider_profile_version": "v1",
            "adapter_version": "compatible-provider-v0.1",
        },
    }
    contract_bytes = json.dumps(contract, separators=(",", ":")).encode()
    contract_hash = "sha256:" + hashlib.sha256(contract_bytes).hexdigest()
    conn.execute(
        "INSERT INTO tasks VALUES(?,?,?,?,?,?,?,?,?)",
        (TASK_ID, OBJECTIVE, "failed", "provider_failure", "provider failure: uncertain_reached",
         0, json.dumps(config), contract_bytes.decode(), contract_hash),
    )
    conn.execute(
        "INSERT INTO provider_attempts VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
        ("exec-000001", TASK_ID, TASK_ID + "-0001", config["provider_id"], "instant", "openai/gpt-oss-120b",
         1, "uncertain", "uncertain_reached", 1, 1, 1, 0, "sent_unconfirmed", "openai_compatible",
         CONFIG_IDENTITY, "", "", "2026-10-04T14:09:06.930650992Z", "2026-10-04T14:09:06.931818943Z"),
    )
    conn.execute("INSERT INTO governor_ledger VALUES(2, ?, ?)", (TASK_ID, "2026-10-04T14:09:06.930650992Z"))
    conn.execute("INSERT INTO governor_task_states VALUES(?, 1, 0)", (TASK_ID,))
    acceptance = json.dumps(
        {"version": 1, "checks": [{
            "id": "calc-source-unchanged", "type": "file_hash", "path": "app/calc.go",
            "sha256": "b8a1bd5dc67bfd9a64bd13503994fdf5e3fc78edaf80502f29992561c6986d94",
        }]}, separators=(",", ":"))
    conn.execute("INSERT INTO acceptance_plans VALUES(?, 1, ?, ?, ?)",
                 (TASK_ID, acceptance, ACCEPTANCE_DIGEST, "2026-10-04T14:09:06.930455989Z"))
    rows = [
        (1, "task_created", {"status": "planned"}, "2026-10-04T14:09:06.930400000Z"),
        (2, "task_started", {"status": "running"}, "2026-10-04T14:09:06.930440000Z"),
        (3, "acceptance_plan_saved", {"digest": ACCEPTANCE_DIGEST}, "2026-10-04T14:09:06.930455989Z"),
        (4, "provider_attempt_prepared", {"execution_id": "exec-000001", "client_request_id": TASK_ID + "-0001",
          "provider": config["provider_id"], "model": config["provider_model"], "model_pool": "instant",
          "protocol_family": "openai_compatible", "config_identity": CONFIG_IDENTITY, "attempt_sequence": 1,
          "governor": {"next_attempt": 2, "task_used": 1, "rolling_10m": 1, "rolling_1h": 1, "rolling_3h": 1}},
         "2026-10-04T14:09:06.930654572Z"),
        (5, "provider_attempt_uncertain", {"client_request_id": TASK_ID + "-0001", "status": "uncertain",
          "outcome": "uncertain_reached", "upstream_reached": True, "uncertain": True,
          "delivery_state": "sent_unconfirmed", "attempt_debited": 1, "selected_backoff": 0,
          "protocol_family": "openai_compatible", "config_identity": CONFIG_IDENTITY,
          "receipts": 0, "receipt_error": "",
          "governor": {"next_attempt": 2, "task_used": 1, "rolling_10m": 1, "rolling_1h": 1, "rolling_3h": 1}},
         "2026-10-04T14:09:06.931818943Z"),
        (6, "task_finalized", {"status": "failed", "outcome": "provider_failure"}, "2026-10-04T14:09:06.932000000Z"),
    ]
    conn.executemany("INSERT INTO events VALUES(?,?,?,?,?)",
                     [(TASK_ID, seq, kind, json.dumps(payload), at) for seq, kind, payload, at in rows])
    conn.commit()
    conn.close()
    return state


class Stage2AuditTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.state_dir = create_expected_state(self.root)

    def tearDown(self):
        self.temp.cleanup()

    def mutate(self, sql, values=()):
        conn = sqlite3.connect(self.state_dir / "runstead.db")
        conn.execute(sql, values)
        conn.commit()
        conn.close()

    def test_expected_trajectory_passes(self):
        before = hashlib.sha256((self.state_dir / "runstead.db").read_bytes()).digest()
        result = stage2_audit.audit_state(self.state_dir, TASK_ID, secret="synthetic-secret")
        after = hashlib.sha256((self.state_dir / "runstead.db").read_bytes()).digest()
        self.assertEqual(result["audit_result"], "PASS")
        self.assertEqual(result["secret_scan"], "clean")
        self.assertEqual(result["provider_attempts"], 1)
        self.assertEqual(after, before)

    def test_acceptance_resaved_after_dispatch_fails_closed(self):
        conn = sqlite3.connect(self.state_dir / "runstead.db")
        conn.execute(
            "INSERT INTO events VALUES(?,?,?,?,?)",
            (TASK_ID, 7, "acceptance_plan_saved", json.dumps({"digest": ACCEPTANCE_DIGEST}),
             "2026-10-04T14:09:06.940000000Z"),
        )
        conn.commit()
        conn.close()
        with self.assertRaises(stage2_audit.AuditError):
            stage2_audit.audit_state(self.state_dir, TASK_ID, secret="synthetic-secret")

    def test_second_physical_attempt_fails_closed(self):
        self.mutate(
            "INSERT INTO provider_attempts SELECT 'exec-000002',task_id,client_request_id||'-2',provider,model_pool,model,2,'uncertain',outcome,1,1,1,selected_backoff_ns,delivery_state,protocol_family,config_identity,'','',prepared_at,completed_at FROM provider_attempts WHERE attempt_sequence=1"
        )
        with self.assertRaises(stage2_audit.AuditError):
            stage2_audit.audit_state(self.state_dir, TASK_ID, secret="synthetic-secret")

    def test_divergent_debit_fails_closed(self):
        self.mutate("UPDATE provider_attempts SET attempt_debited=0")
        with self.assertRaises(stage2_audit.AuditError):
            stage2_audit.audit_state(self.state_dir, TASK_ID, secret="synthetic-secret")

    def test_unexpected_action_or_tool_fails_closed(self):
        for table, columns, values in (
            ("actions", "task_id,tool", (TASK_ID, "read_file")),
            ("tool_attempts", "task_id,tool", (TASK_ID, "read_file")),
        ):
            with self.subTest(table=table):
                self.mutate(f"INSERT INTO {table}({columns}) VALUES(?,?)", values)
                with self.assertRaises(stage2_audit.AuditError):
                    stage2_audit.audit_state(self.state_dir, TASK_ID, secret="synthetic-secret")
                self.mutate(f"DELETE FROM {table}")

    def test_unexpected_tool_result_fails_closed(self):
        self.mutate("INSERT INTO tool_results VALUES(?)", (TASK_ID,))
        with self.assertRaises(stage2_audit.AuditError):
            stage2_audit.audit_state(self.state_dir, TASK_ID, secret="synthetic-secret")

    def test_unexpected_verifier_fails_closed(self):
        self.mutate("INSERT INTO verification_attempts VALUES(?)", (TASK_ID,))
        with self.assertRaises(stage2_audit.AuditError):
            stage2_audit.audit_state(self.state_dir, TASK_ID, secret="synthetic-secret")

    def test_different_delivery_state_fails_closed(self):
        self.mutate("UPDATE provider_attempts SET delivery_state='response_started'")
        with self.assertRaises(stage2_audit.AuditError):
            stage2_audit.audit_state(self.state_dir, TASK_ID, secret="synthetic-secret")

    def test_provider_family_or_model_divergence_fails_closed(self):
        for column, value in (("provider", "other-provider"), ("protocol_family", "other-family"),
                              ("model", "other-model"), ("config_identity", "other-config")):
            with self.subTest(column=column):
                isolated = self.root / column
                isolated.mkdir()
                self.state_dir = create_expected_state(isolated)
                self.mutate(f"UPDATE provider_attempts SET {column}=?", (value,))
                with self.assertRaises(stage2_audit.AuditError):
                    stage2_audit.audit_state(self.state_dir, TASK_ID, secret="synthetic-secret")

    def test_secret_found_fails_without_echoing_value(self):
        secret = "synthetic-secret-never-print"
        (self.state_dir / "retained.txt").write_text("before " + secret + " after", encoding="utf-8")
        with self.assertRaises(stage2_audit.AuditError) as caught:
            stage2_audit.audit_state(self.state_dir, TASK_ID, secret=secret)
        self.assertNotIn(secret, str(caught.exception))

    def test_unavailable_secret_scan_marks_result_limited(self):
        result = stage2_audit.audit_state(self.state_dir, TASK_ID, secret=None)
        self.assertEqual(result["secret_scan"], "unavailable")
        self.assertEqual(result["audit_result"], "LIMITED")


if __name__ == "__main__":
    unittest.main()
