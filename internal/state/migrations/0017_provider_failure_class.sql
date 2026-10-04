-- Preserve the compatible adapter's sanitized failure taxonomy separately
-- from the governor outcome and the historical receipt-validation error_class.
ALTER TABLE provider_attempts ADD COLUMN provider_failure_class TEXT NOT NULL DEFAULT '' CHECK (
    provider_failure_class IN ('', 'config_refused', 'auth_unavailable',
        'authentication_denied', 'permission_denied', 'rate_or_capacity',
        'timeout', 'cancelled', 'malformed_response', 'invalid_envelope',
        'empty_response', 'unsupported_response_format', 'incomplete_completion',
        'refusal', 'response_too_large', 'request_too_large',
        'upstream_server_failure', 'upstream_http_failure', 'unsafe_redirect',
        'transport')
);
