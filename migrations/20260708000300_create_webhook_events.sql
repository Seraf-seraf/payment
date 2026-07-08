-- +goose Up
CREATE TABLE webhook_events (
    id UUID PRIMARY KEY,
    provider_name TEXT NOT NULL,
    provider_event_id TEXT NOT NULL,
    provider_payment_id TEXT,
    event_type TEXT NOT NULL,
    raw_payload JSONB NOT NULL,
    signature_valid BOOLEAN NOT NULL,
    processed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider_name, provider_event_id)
);

-- +goose Down
DROP TABLE webhook_events;
