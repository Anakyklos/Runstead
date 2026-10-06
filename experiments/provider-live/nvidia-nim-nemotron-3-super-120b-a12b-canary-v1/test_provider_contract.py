import json
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent


class NVIDIAProviderContractTests(unittest.TestCase):
    def test_exact_provider_identity_and_safe_route(self):
        data = json.loads((HERE / "providers.json").read_text(encoding="utf-8"))
        self.assertEqual(data["version"], 1)
        self.assertEqual(len(data["providers"]), 1)
        provider = data["providers"][0]
        self.assertEqual(provider["provider_id"], "nvidia-nim-nemotron-3-super-120b-a12b-canary-v1")
        self.assertEqual(provider["protocol_family"], "openai_compatible")
        self.assertEqual(provider["base_url"], "https://integrate.api.nvidia.com/v1")
        self.assertEqual(provider["model"], "nvidia/nemotron-3-super-120b-a12b")
        self.assertEqual(provider["auth_ref"], "NVIDIA_API_KEY")
        self.assertEqual(provider["auth_requirement"], "reference_required")
        self.assertEqual(provider["profile"]["route_safety"], {
            "attempt_accounting": "single",
            "single_attempt": "guaranteed",
            "internal_retries": "disabled",
            "cooldown_replay": "disabled",
            "account_pooling": "disabled",
            "automatic_fallback": "disabled",
            "combo_routing": "disabled",
        })

    def test_stage1_contract_uses_one_direct_pre_task_control_after_fresh_preflight(self):
        data = json.loads((HERE / "stage1-preflight.json").read_text(encoding="utf-8"))
        self.assertEqual(data["provider_id"], "nvidia-nim-nemotron-3-super-120b-a12b-canary-v1")
        self.assertEqual(data["objective"], "Resolve the exact provider declaration with SafeRouteSafety without constructing an adapter or sending a request. After fresh NVIDIA documentation and credential preflight pass, execute exactly one sanitized direct HTTP control before any Runstead task; this control does not prove the runtime adapter.")
        self.assertEqual(data["stage1_dispatch_mode"], "direct_pre_task_http_control")
        self.assertEqual(data["request_contract"], {
            "method": "POST",
            "path": "/chat/completions",
            "model": "nvidia/nemotron-3-super-120b-a12b",
            "messages": [{"role": "user", "content": "Reply with OK."}],
            "max_tokens": 1,
            "stream": False,
        })
        self.assertEqual(data["maximum_control_requests"], 1)
        self.assertTrue(data["fresh_preflight_required_before_dispatch"])
        self.assertTrue(data["stage1_dispatch_authorized"])
        self.assertTrue(data["stop_later_stages_on_stage1_failure_or_block"])
        self.assertFalse(data["secret_value_emitted"])

    def test_stage2_to_stage4_contracts_keep_order_and_stage4_resume_same_task(self):
        stage2 = json.loads((HERE / "stage2-preflight.json").read_text(encoding="utf-8"))
        stage3 = json.loads((HERE / "stage3-preflight.json").read_text(encoding="utf-8"))
        stage4 = json.loads((HERE / "stage4-preflight.json").read_text(encoding="utf-8"))
        self.assertEqual(stage2["requires_previous_stage_pass"], "stage1")
        self.assertEqual(stage2["objective"], "Read app/calc.go using the available repository tool. Complete only after citing the actual read_file observation for app/calc.go. Do not modify files and do not run recipes/processes.")
        self.assertEqual(stage3["requires_previous_stage_pass"], "stage2")
        self.assertEqual(stage3["objective"], "Fix the whitespace handling bug in app/calc.go so the test suite passes. Inspect relevant code and tests, make the minimum scoped change, run the declared test recipe, and finish only after independent Runstead acceptance verification passes.")
        self.assertEqual(stage4["requires_previous_stage_pass"], "stage3")
        self.assertTrue(stage4["same_task_id_required"])
        self.assertTrue(stage4["resume_after_read_only_inspect"])
        self.assertEqual(stage4["interrupt_checkpoint"], "after the successful declared test recipe is durable and before final verification begins")


if __name__ == "__main__":
    unittest.main()
