-- +goose Up
CREATE TABLE invoice_inbox (
    id UUID PRIMARY KEY,
    connection_id TEXT NOT NULL CHECK (btrim(connection_id) <> ''),
    event_key TEXT NOT NULL CHECK (btrim(event_key) <> ''),
    raw_body_hash BYTEA NOT NULL CHECK (octet_length(raw_body_hash) = 32),
    normalized_payload JSONB NOT NULL CHECK (jsonb_typeof(normalized_payload) = 'object'),
    verification_kind TEXT NOT NULL CHECK (btrim(verification_kind) <> ''),
    external_invoice_id TEXT CHECK (
        external_invoice_id IS NULL OR btrim(external_invoice_id) <> ''
    ),
    reference TEXT CHECK (
        reference IS NULL OR reference ~ '^INV-[0-9A-F]{32}$'
    ),
    state TEXT NOT NULL DEFAULT 'received' CHECK (
        state IN ('received', 'processed', 'unmatched', 'review')
    ),
    received_at TIMESTAMPTZ NOT NULL,
    processed_at TIMESTAMPTZ,
    error_code TEXT CHECK (error_code IS NULL OR btrim(error_code) <> ''),
    next_match_at TIMESTAMPTZ,
    CONSTRAINT invoice_inbox_match_schedule_check CHECK (
        (state = 'unmatched') = (next_match_at IS NOT NULL)
    )
);

CREATE UNIQUE INDEX idx_invoice_inbox_connection_event_body
    ON invoice_inbox (connection_id, event_key, raw_body_hash);

CREATE INDEX idx_invoice_inbox_match_external_id
    ON invoice_inbox (connection_id, external_invoice_id)
    WHERE state = 'unmatched' AND external_invoice_id IS NOT NULL;

CREATE INDEX idx_invoice_inbox_match_reference
    ON invoice_inbox (connection_id, reference)
    WHERE state = 'unmatched' AND reference IS NOT NULL;

CREATE INDEX idx_invoice_inbox_match_due
    ON invoice_inbox (next_match_at, id)
    WHERE state = 'unmatched';

CREATE TABLE invoice_commands (
    id UUID PRIMARY KEY,
    command_type TEXT NOT NULL CHECK (
        command_type IN ('invoice.issue', 'invoice.apply_notification', 'invoice.reconcile')
    ),
    payment_id UUID REFERENCES invoice_payments(payment_id) ON DELETE RESTRICT,
    inbox_id UUID REFERENCES invoice_inbox(id) ON DELETE RESTRICT,
    status TEXT NOT NULL CHECK (status IN ('pending', 'processing', 'completed', 'failed')),
    attempts INT NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL,
    lease_token UUID,
    lease_expires_at TIMESTAMPTZ,
    last_error_code TEXT CHECK (last_error_code IS NULL OR btrim(last_error_code) <> ''),
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ,
    CONSTRAINT invoice_commands_target_check CHECK (
        (
            command_type IN ('invoice.issue', 'invoice.reconcile')
            AND payment_id IS NOT NULL
            AND inbox_id IS NULL
        )
        OR (
            command_type = 'invoice.apply_notification'
            AND payment_id IS NULL
            AND inbox_id IS NOT NULL
        )
    ),
    CONSTRAINT invoice_commands_lease_check CHECK (
        (status = 'processing' AND lease_token IS NOT NULL AND lease_expires_at IS NOT NULL)
        OR (status <> 'processing' AND lease_token IS NULL AND lease_expires_at IS NULL)
    )
);

CREATE UNIQUE INDEX invoice_command_one_issue
    ON invoice_commands (payment_id)
    WHERE command_type = 'invoice.issue';

CREATE UNIQUE INDEX invoice_command_one_active_reconcile
    ON invoice_commands (payment_id)
    WHERE command_type = 'invoice.reconcile' AND status IN ('pending', 'processing');

CREATE UNIQUE INDEX invoice_command_one_active_apply
    ON invoice_commands (inbox_id)
    WHERE command_type = 'invoice.apply_notification' AND status IN ('pending', 'processing');

CREATE INDEX idx_invoice_commands_pending
    ON invoice_commands (next_attempt_at, id)
    WHERE status = 'pending';

CREATE INDEX idx_invoice_commands_expired_lease
    ON invoice_commands (lease_expires_at, id)
    WHERE status = 'processing';

-- +goose Down
LOCK TABLE invoice_commands, invoice_inbox IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM invoice_commands)
       OR EXISTS (SELECT 1 FROM invoice_inbox) THEN
        RAISE EXCEPTION 'refusing to drop invoice inbox or command data'
            USING ERRCODE = '55000';
    END IF;
END;
$$;
-- +goose StatementEnd

DROP INDEX idx_invoice_commands_expired_lease;
DROP INDEX idx_invoice_commands_pending;
DROP INDEX invoice_command_one_active_apply;
DROP INDEX invoice_command_one_active_reconcile;
DROP INDEX invoice_command_one_issue;
DROP INDEX idx_invoice_inbox_match_due;
DROP INDEX idx_invoice_inbox_match_reference;
DROP INDEX idx_invoice_inbox_match_external_id;
DROP INDEX idx_invoice_inbox_connection_event_body;
DROP TABLE invoice_commands;
DROP TABLE invoice_inbox;
