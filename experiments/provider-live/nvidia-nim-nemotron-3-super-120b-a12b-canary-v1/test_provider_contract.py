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

    def test_stage1_contract_is_exact_and_dispatch_is_stopped(self):
        data = json.loads((HERE / "stage1-preflight.json").read_text(encoding="utf-8"))
        self.assertEqual(data["provider_id"], "nvidia-nim-nemotron-3-super-120b-a12b-canary-v1")
        self.assertEqual(data["request_contract_if_adapter_supports_it"], {
            "method": "POST",
            "path": "/chat/completions",
            "model": "nvidia/nemotron-3-super-120b-a12b",
            "messages": [{"role": "user", "content": "Reply with OK."}],
            "max_tokens": 1,
            "stream": False,
        })
        self.assertEqual(data["maximum_control_requests"], 1)
        self.assertFalse(data["stage1_dispatch_authorized"])
        self.assertTrue(data["stop_later_stages_on_stage1_failure_or_block"])
        self.assertFalse(data["secret_value_emitted"])


if __name__ == "__main__":
    unittest.main()
