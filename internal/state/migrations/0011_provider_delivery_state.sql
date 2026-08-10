-- Provider delivery evidence for issue #38. Empty is intentional: TX1 has
-- not observed transport delivery yet, and recovery must not fabricate it.
ALTER TABLE provider_attempts ADD COLUMN delivery_state TEXT NOT NULL DEFAULT '' CHECK (
    delivery_state IN ('', 'not_sent', 'sent_confirmed', 'sent_unconfirmed',
                       'response_started', 'completed')
);
