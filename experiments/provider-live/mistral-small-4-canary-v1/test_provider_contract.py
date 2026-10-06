import json
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent


class MistralProviderContractTests(unittest.TestCase):
    def test_provider_declaration_is_exact_and_safe_route(self):
        data = json.loads((HERE / "providers.json").read_text(encoding="utf-8"))
        self.assertEqual(data["version"], 1)
        self.assertEqual(len(data["providers"]), 1)
        provider = data["providers"][0]
        self.assertEqual(provider["provider_id"], "mistral-small-4-canary-v1")
        self.assertEqual(provider["protocol_family"], "openai_compatible")
        self.assertEqual(provider["base_url"], "https://api.mistral.ai/v1")
        self.assertEqual(provider["model"], "mistral-small-2603")
        self.assertEqual(provider["auth_ref"], "MISTRAL_API_KEY")
        self.assertEqual(provider["auth_requirement"], "reference_required")
        safety = provider["profile"]["route_safety"]
        self.assertEqual(safety, {
            "attempt_accounting": "single",
            "single_attempt": "guaranteed",
            "internal_retries": "disabled",
            "cooldown_replay": "disabled",
            "account_pooling": "disabled",
            "automatic_fallback": "disabled",
            "combo_routing": "disabled",
        })

    def test_stage1_control_request_is_minimal_and_single_shot(self):
        data = json.loads((HERE / "stage1-preflight.json").read_text(encoding="utf-8"))
        self.assertEqual(data["provider_id"], "mistral-small-4-canary-v1")
        self.assertEqual(data["request"], {
            "method": "POST",
            "path": "/v1/chat/completions",
            "model": "mistral-small-2603",
            "messages": [{"role": "user", "content": "Reply with OK."}],
            "max_tokens": 1,
            "stream": False,
        })
        self.assertEqual(data["maximum_control_requests"], 1)
        self.assertTrue(data["stop_later_stages_on_failure"])
        self.assertNotIn("temperature", data["request"])
        self.assertNotIn("reasoning_effort", data["request"])


if __name__ == "__main__":
    unittest.main()
