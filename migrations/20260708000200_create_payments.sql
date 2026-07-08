-- +goose Up
CREATE TABLE payments (
    id UUID PRIMARY KEY,
    merchant_id UUID NOT NULL REFERENCES merchants(id),
    provider_name TEXT NOT NULL,
    provider_payment_id TEXT,
    merchant_order_id TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    amount_minor BIGINT NOT NULL,
    currency TEXT NOT NULL,
    status TEXT NOT NULL,
    payment_url TEXT,
    metadata JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    paid_at TIMESTAMPTZ,
    canceled_at TIMESTAMPTZ,
    refunded_at TIMESTAMPTZ,
    UNIQUE (merchant_id, idempotency_key),
    UNIQUE (provider_name, provider_payment_id)
);

CREATE INDEX idx_payments_merchant_order_id ON payments (merchant_id, merchant_order_id);
CREATE INDEX idx_payments_status ON payments (status);

-- +goose Down
DROP TABLE payments;
