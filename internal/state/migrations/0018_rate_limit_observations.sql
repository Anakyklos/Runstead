-- Preserve bounded, sanitized provider rate-limit observations separately
-- from governor-selected retry/backoff and provider/receipt failure classes.
ALTER TABLE provider_attempts ADD COLUMN status_code INTEGER;
ALTER TABLE provider_attempts ADD COLUMN observed_reset_at TEXT;
ALTER TABLE provider_attempts ADD COLUMN observed_retry_after_ns INTEGER;
ALTER TABLE provider_attempts ADD COLUMN limit_requests INTEGER;
ALTER TABLE provider_attempts ADD COLUMN remaining_requests INTEGER;
ALTER TABLE provider_attempts ADD COLUMN reset_requests_ns INTEGER;
ALTER TABLE provider_attempts ADD COLUMN limit_tokens INTEGER;
ALTER TABLE provider_attempts ADD COLUMN remaining_tokens INTEGER;
ALTER TABLE provider_attempts ADD COLUMN reset_tokens_ns INTEGER;
