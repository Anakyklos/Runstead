import json
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent


class ProviderContractTests(unittest.TestCase):
    def test_v2_provider_declaration_is_exact_and_retry_free(self):
        data = json.loads((HERE / "providers.json").read_text(encoding="utf-8"))
        self.assertEqual(len(data["providers"]), 1)
        provider = data["providers"][0]
        self.assertEqual(provider["provider_id"], "nvidia-nim-laguna-xs-2-1-canary-v2")
        self.assertEqual(provider["protocol_family"], "openai_compatible")
        self.assertEqual(provider["base_url"], "https://integrate.api.nvidia.com/v1")
        self.assertEqual(provider["model"], "poolside/laguna-xs-2.1")
        self.assertEqual(provider["auth_ref"], "NVIDIA_API_KEY")
        safety = provider["profile"]["route_safety"]
        self.assertEqual(safety["internal_retries"], "disabled")
        self.assertEqual(safety["automatic_fallback"], "disabled")
        self.assertEqual(safety["account_pooling"], "disabled")
        self.assertEqual(safety["combo_routing"], "disabled")

    def test_stage1_contract_has_exact_minimal_request_and_stops_on_failure(self):
        data = json.loads((HERE / "stage1-preflight.json").read_text(encoding="utf-8"))
        self.assertEqual(data["provider_id"], "nvidia-nim-laguna-xs-2-1-canary-v2")
        self.assertEqual(data["protocol_family"], "openai_compatible")
        self.assertEqual(data["model"], "poolside/laguna-xs-2.1")
        self.assertEqual(data["auth_ref"], "NVIDIA_API_KEY")
        self.assertEqual(data["request"], {
            "method": "POST",
            "path": "/chat/completions",
            "model": "poolside/laguna-xs-2.1",
            "user_content": "Reply with OK.",
            "max_tokens": 1,
            "stream": False,
        })
        self.assertEqual(data["maximum_control_requests"], 1)
        self.assertTrue(data["stop_later_stages_on_failure"])


if __name__ == "__main__":
    unittest.main()
