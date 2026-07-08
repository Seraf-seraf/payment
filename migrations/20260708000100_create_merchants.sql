-- +goose Up
CREATE TABLE merchants (
    id UUID PRIMARY KEY,
    name TEXT NOT NULL,
    api_key_hash TEXT NOT NULL UNIQUE,
    shared_secret_encrypted TEXT NOT NULL,
    callback_url TEXT NOT NULL,
    success_url TEXT,
    fail_url TEXT,
    provider_name TEXT NOT NULL,
    is_active BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE merchants;
