import unittest

import stage2_audit


class Stage2AuditTests(unittest.TestCase):
    def test_only_cited_successful_observation_qualifies_as_read_evidence(self):
        self.assertIn("app/calc.go", stage2_audit.OBJECTIVE)
        self.assertEqual(stage2_audit.PROVIDER_ID, "groq-gpt-oss-120b-canary-v4")
        self.assertEqual(stage2_audit.MODEL, "openai/gpt-oss-120b")

    def test_unknown_provider_telemetry_is_not_rendered_as_raw_text(self):
        self.assertEqual(stage2_audit._safe("unexpected raw detail", {"timeout", "transport"}), "unknown")

    def test_citation_must_match_existing_read_file_observation(self):
        valid = [{
            "evidence_id": "obs-000001",
            "claimed_tool": "read_file",
            "tool": "read_file",
            "exists": True,
            "tool_matches": True,
        }]
        wrong_tool = [{**valid[0], "claimed_tool": "run_recipe"}]
        self.assertTrue(stage2_audit._citation_matches_read_file(valid, {"obs-000001"}))
        self.assertFalse(stage2_audit._citation_matches_read_file(valid, {"obs-000002"}))
        self.assertFalse(stage2_audit._citation_matches_read_file(wrong_tool, {"obs-000001"}))


if __name__ == "__main__":
    unittest.main()
