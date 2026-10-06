import json
import unittest

import stage2_audit
import stage3_audit
import stage4_runner
import stage4_audit


class StageAuditTests(unittest.TestCase):
    def test_stage2_citation_must_match_persisted_read_file_observation(self):
        good = [{
            "evidence_id": "obs-000017",
            "claimed_tool": "read_file",
            "tool": "read_file",
            "exists": True,
            "tool_matches": True,
        }]
        self.assertTrue(stage2_audit._citation_matches_read_file(good, {"obs-000017"}))
        self.assertFalse(stage2_audit._citation_matches_read_file(good, {"obs-000018"}))
        self.assertFalse(stage2_audit._citation_matches_read_file(
            [{**good[0], "tool": "list_files"}], {"obs-000017"}
        ))

    def test_stage3_write_scope_is_only_calc_source(self):
        self.assertTrue(stage3_audit.scoped("app/calc.go"))
        self.assertFalse(stage3_audit.scoped("app/calc_test.go"))
        self.assertFalse(stage3_audit.scoped("README.md"))
        self.assertFalse(stage3_audit.scoped("../outside.go"))

    def test_stage3_recipe_pass_requires_declared_untruncated_success(self):
        good = {
            "recipe_id": "test",
            "started": True,
            "exit_code": 0,
            "timed_out": False,
            "canceled": False,
            "stdout_truncated": False,
            "stderr_truncated": False,
        }
        self.assertTrue(stage3_audit.recipe_passed(json.dumps(good)))
        self.assertFalse(stage3_audit.recipe_passed(json.dumps({**good, "recipe_id": "other"})))
        self.assertFalse(stage3_audit.recipe_passed(json.dumps({**good, "exit_code": 1})))
        self.assertFalse(stage3_audit.recipe_passed(json.dumps({**good, "stdout_truncated": True})))

    def test_stage4_interrupt_waits_for_durable_write_and_recipe_before_verification(self):
        ready = {
            "task_status": "running",
            "objective_matches": True,
            "target_write_durable": True,
            "target_hash_matches": True,
            "successful_test_recipe_durable": True,
            "acceptance_digest": "sha256:synthetic",
            "verification_attempts": 0,
            "unsettled_provider_attempts": 0,
        }
        self.assertTrue(stage4_runner.checkpoint_ready(ready))
        for key, value in (
            ("target_hash_matches", False),
            ("successful_test_recipe_durable", False),
            ("verification_attempts", 1),
            ("unsettled_provider_attempts", 1),
        ):
            with self.subTest(key=key):
                self.assertFalse(stage4_runner.checkpoint_ready({**ready, key: value}))

    def test_stage4_audit_rejects_effect_replay_and_uncertain_attempt_replay(self):
        before = {
            "task_id": "cli-123456789",
            "task_status": "running",
            "objective_matches": True,
            "workspace_matches": True,
            "model_matches": True,
            "provider_id_matches": True,
            "provider_model_matches": True,
            "provider_protocol_matches": True,
            "config_sha256": "sha256:config",
            "execution_contract_hash": "sha256:contract",
            "acceptance_digest": "sha256:acceptance",
            "resume_count": 0,
            "provider_attempts": [{
                "execution_id": "p1", "client_request_id": "cli-123456789-0001",
                "status": "completed", "delivery_state": "completed", "upstream_reached": 1,
                "uncertain": 0, "attempt_debited": 1, "attempt_sequence": 1,
            }],
            "provider_attempt_count": 1,
            "governor_admissions": 1,
            "attempt_debits": 1,
            "governor_attempts": 1,
            "governor_retries": 0,
            "unsettled_provider_attempts": 0,
            "tool_attempts": [
                {"execution_id": "w1", "tool": "write_file"},
                {"execution_id": "r1", "tool": "run_recipe"},
            ],
            "target_write_durable": True,
            "target_hash_matches": True,
            "successful_test_recipe_durable": True,
            "evidence_ids": ["obs-000001"],
            "verification_attempts": 0,
            "latest_verification": "none",
        }
        after = {
            **before,
            "task_status": "completed",
            "resume_count": 1,
            "provider_attempts": before["provider_attempts"] + [{
                "execution_id": "p2", "client_request_id": "cli-123456789-0002",
                "status": "completed", "delivery_state": "completed", "upstream_reached": 1,
                "uncertain": 0, "attempt_debited": 1, "attempt_sequence": 2,
            }],
            "provider_attempt_count": 2,
            "governor_admissions": 2,
            "attempt_debits": 2,
            "governor_attempts": 2,
            "tool_attempts": before["tool_attempts"] + [{"execution_id": "v1", "tool": "read_file"}],
            "evidence_ids": ["obs-000001", "obs-000002"],
            "verification_attempts": 1,
            "latest_verification": "passed",
        }
        control = {
            "task_id": "cli-123456789", "interruption_signal": "SIGKILL",
            "inspect_exit_code": 0, "resume_exit_code": 0,
        }
        self.assertEqual(stage4_audit.reconcile_snapshots(before, after, control), [])
        replayed = {**after, "tool_attempts": after["tool_attempts"] + [{"execution_id": "w2", "tool": "write_file"}]}
        self.assertIn("completed_effect_replayed", stage4_audit.reconcile_snapshots(before, replayed, control))
        uncertain = {**before, "provider_attempts": [{**before["provider_attempts"][0], "delivery_state": "sent_unconfirmed", "uncertain": 1}]}
        self.assertIn("uncertain_delivery_reached_checkpoint", stage4_audit.reconcile_snapshots(uncertain, after, control))


if __name__ == "__main__":
    unittest.main()
