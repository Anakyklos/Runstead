import unittest

import stage3_audit


class Stage3ObservationTests(unittest.TestCase):
    def test_absent_provider_observations_render_unknown(self):
        rendered = stage3_audit.render_rate_limit_observation({
            "status_code": None,
            "observed_reset_at": None,
            "observed_retry_after_ns": None,
            "limit_requests": None,
            "remaining_requests": None,
            "reset_requests_ns": None,
            "limit_tokens": None,
            "remaining_tokens": None,
            "reset_tokens_ns": None,
            "selected_backoff_ns": 0,
            "provider_failure_class": "",
            "delivery_state": "",
        })

        for field in (
            "http_status", "observed_retry_after", "observed_reset_at",
            "limit_requests", "remaining_requests", "reset_requests",
            "limit_tokens", "remaining_tokens", "reset_tokens",
            "provider_failure_class", "delivery_state",
        ):
            self.assertIn(f"{field}=unknown", rendered)
        self.assertIn("selected_backoff=0s", rendered)

    def test_provider_observation_and_governor_backoff_stay_distinct(self):
        rendered = stage3_audit.render_rate_limit_observation({
            "status_code": 429,
            "observed_reset_at": "2026-10-05T13:00:00Z",
            "observed_retry_after_ns": 7_000_000_000,
            "limit_requests": 120,
            "remaining_requests": 0,
            "reset_requests_ns": 60_000_000_000,
            "limit_tokens": 8_000,
            "remaining_tokens": 0,
            "reset_tokens_ns": 60_000_000_000,
            "selected_backoff_ns": 29_000_000_000,
            "provider_failure_class": "rate_or_capacity",
            "delivery_state": "completed",
        })

        self.assertIn("http_status=429", rendered)
        self.assertIn("observed_retry_after=7s", rendered)
        self.assertIn("remaining_requests=0", rendered)
        self.assertIn("selected_backoff=29s", rendered)
        self.assertIn("provider_failure_class=rate_or_capacity", rendered)
        self.assertIn("delivery_state=completed", rendered)
        self.assertIn("observation_vs_governor=separate", rendered)
        self.assertIn("observations_are_diagnostic_only=true", rendered)
        self.assertIn("no_limit_dimension_inferred=true", rendered)

    def test_invalid_observation_values_fail_closed_to_unknown(self):
        rendered = stage3_audit.render_rate_limit_observation({
            "status_code": "429 raw",
            "observed_reset_at": "2026-99-99T13:00:00Z",
            "observed_retry_after_ns": 0,
            "limit_requests": 0,
            "remaining_requests": -1,
            "reset_requests_ns": -1,
            "limit_tokens": 1_000_000_001,
            "remaining_tokens": 0,
            "reset_tokens_ns": 60_000_000_000,
            "selected_backoff_ns": -5,
            "provider_failure_class": "raw-provider-error",
            "delivery_state": "raw-delivery",
        })

        for field in (
            "http_status", "observed_retry_after", "observed_reset_at",
            "limit_requests", "remaining_requests", "reset_requests",
            "selected_backoff", "provider_failure_class", "delivery_state",
        ):
            self.assertIn(f"{field}=unknown", rendered)
        self.assertIn("limit_tokens=unknown", rendered)
        self.assertIn("remaining_tokens=0", rendered)


if __name__ == "__main__":
    unittest.main()
