-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION invoice_review_reasons_are_valid(reasons TEXT[])
RETURNS BOOLEAN
LANGUAGE plpgsql
IMMUTABLE
STRICT
PARALLEL SAFE
AS $$
DECLARE
    reason TEXT;
    previous_reason TEXT;
BEGIN
    IF cardinality(reasons) = 0 THEN
        RETURN TRUE;
    END IF;

    IF array_ndims(reasons) <> 1 THEN
        RETURN FALSE;
    END IF;

    FOREACH reason IN ARRAY reasons LOOP
        IF reason IS NULL OR reason NOT IN (
            'issuance_unknown',
            'invoice_terms_mismatch',
            'payer_mismatch',
            'currency_mismatch',
            'ambiguous_reference',
            'operation_changed',
            'overpaid',
            'acceptance_deadline_elapsed',
            'financial_correction'
        ) THEN
            RETURN FALSE;
        END IF;

        IF previous_reason IS NOT NULL
           AND previous_reason COLLATE "C" >= reason COLLATE "C" THEN
            RETURN FALSE;
        END IF;

        previous_reason := reason;
    END LOOP;

    RETURN TRUE;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION protect_invoice_payment_identity()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF ROW(
        NEW.payment_id,
        NEW.merchant_id,
        NEW.request_id,
        NEW.request_hash,
        NEW.request_snapshot,
        NEW.connection_id,
        NEW.provider_kind,
        NEW.connection_snapshot,
        NEW.invoice_number,
        NEW.reference,
        NEW.evidence_mode,
        NEW.accept_until,
        NEW.created_at
    ) IS DISTINCT FROM ROW(
        OLD.payment_id,
        OLD.merchant_id,
        OLD.request_id,
        OLD.request_hash,
        OLD.request_snapshot,
        OLD.connection_id,
        OLD.provider_kind,
        OLD.connection_snapshot,
        OLD.invoice_number,
        OLD.reference,
        OLD.evidence_mode,
        OLD.accept_until,
        OLD.created_at
    ) THEN
        RAISE EXCEPTION 'invoice payment identity fields are immutable'
            USING ERRCODE = '55000';
    END IF;

    IF OLD.external_invoice_id IS NOT NULL
       AND NEW.external_invoice_id IS DISTINCT FROM OLD.external_invoice_id THEN
        RAISE EXCEPTION 'external invoice id is immutable once assigned'
            USING ERRCODE = '55000';
    END IF;

    IF OLD.payment_confirmed_at IS NOT NULL
       AND NEW.payment_confirmed_at IS DISTINCT FROM OLD.payment_confirmed_at THEN
        RAISE EXCEPTION 'payment confirmation time is immutable once recorded'
            USING ERRCODE = '55000';
    END IF;

    IF OLD.confirmation_recorded_at IS NOT NULL
       AND NEW.confirmation_recorded_at IS DISTINCT FROM OLD.confirmation_recorded_at THEN
        RAISE EXCEPTION 'confirmation recording time is immutable once recorded'
            USING ERRCODE = '55000';
    END IF;

    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION validate_invoice_payment_parent()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    parent_merchant_id UUID;
    parent_provider_name TEXT;
    parent_provider_payment_id TEXT;
    parent_payment_url TEXT;
    parent_idempotency_key TEXT;
BEGIN
    SELECT merchant_id, provider_name, provider_payment_id, payment_url, idempotency_key
    INTO parent_merchant_id, parent_provider_name, parent_provider_payment_id, parent_payment_url,
         parent_idempotency_key
    FROM payments
    WHERE id = NEW.payment_id
    FOR KEY SHARE;

    IF NOT FOUND THEN
        RETURN NEW;
    END IF;

    IF parent_merchant_id IS DISTINCT FROM NEW.merchant_id
       OR parent_provider_name IS DISTINCT FROM 'invoice:' || NEW.connection_id
       OR parent_provider_payment_id IS NOT NULL
       OR parent_payment_url IS NOT NULL
       OR parent_idempotency_key IS DISTINCT FROM 'invoice:' || NEW.request_id::TEXT THEN
        RAISE EXCEPTION 'invoice payment parent fields do not match invoice identity'
            USING ERRCODE = '23514';
    END IF;

    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION reject_invoice_evidence_mutation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'invoice evidence is immutable'
        USING ERRCODE = '55000';

    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION reject_invoice_payment_delete()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'invoice payment records cannot be deleted'
        USING ERRCODE = '55000';

    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TABLE invoice_payments (
    payment_id UUID PRIMARY KEY REFERENCES payments(id) ON DELETE RESTRICT,
    merchant_id UUID NOT NULL REFERENCES merchants(id) ON DELETE RESTRICT,
    request_id UUID NOT NULL,
    request_hash BYTEA NOT NULL CHECK (octet_length(request_hash) = 32),
    request_snapshot JSONB NOT NULL CHECK (jsonb_typeof(request_snapshot) = 'object'),
    connection_id TEXT NOT NULL CHECK (btrim(connection_id) <> ''),
    provider_kind TEXT NOT NULL CHECK (btrim(provider_kind) <> ''),
    connection_snapshot JSONB NOT NULL CHECK (jsonb_typeof(connection_snapshot) = 'object'),
    invoice_number BIGINT GENERATED ALWAYS AS IDENTITY (MINVALUE 1 MAXVALUE 999999999999999),
    reference TEXT NOT NULL,
    external_invoice_id TEXT CHECK (external_invoice_id IS NULL OR btrim(external_invoice_id) <> ''),
    evidence_mode TEXT NOT NULL CHECK (evidence_mode IN ('invoice_confirmation', 'bank_credits')),
    issuance_state TEXT NOT NULL CHECK (
        issuance_state IN ('queued', 'dispatched', 'issued', 'unknown', 'rejected', 'expired_before_issue')
    ),
    funding_state TEXT NOT NULL CHECK (funding_state IN ('none', 'partial', 'paid', 'overpaid')),
    credited_minor BIGINT NOT NULL DEFAULT 0 CHECK (credited_minor BETWEEN 0 AND 9007199254740991),
    review_reasons TEXT[] NOT NULL DEFAULT '{}'::TEXT[]
        CHECK (invoice_review_reasons_are_valid(review_reasons)),
    revision BIGINT NOT NULL DEFAULT 1 CHECK (revision > 0),
    accept_until TIMESTAMPTZ NOT NULL,
    provider_expires_at TIMESTAMPTZ,
    dispatched_at TIMESTAMPTZ,
    payment_confirmed_at TIMESTAMPTZ,
    confirmation_recorded_at TIMESTAMPTZ,
    artifact JSONB CHECK (artifact IS NULL OR jsonb_typeof(artifact) = 'object'),
    next_reconcile_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT invoice_payments_request_per_merchant_unique UNIQUE (merchant_id, request_id),
    CONSTRAINT invoice_payments_invoice_number_unique UNIQUE (invoice_number),
    CONSTRAINT invoice_payments_invoice_number_range CHECK (
        invoice_number BETWEEN 1 AND 999999999999999
    ),
    CONSTRAINT invoice_payments_reference_unique UNIQUE (reference),
    CONSTRAINT invoice_payments_reference_matches_payment CHECK (
        reference = 'INV-' || replace(upper(payment_id::TEXT), '-', '')
    ),
    CONSTRAINT invoice_payments_request_snapshot_deadline CHECK (
        request_snapshot ? 'accept_until'
        AND jsonb_typeof(request_snapshot->'accept_until') = 'string'
    ),
    CONSTRAINT invoice_payments_identity_scope_unique UNIQUE (payment_id, connection_id, evidence_mode)
);

CREATE UNIQUE INDEX idx_invoice_payments_external_invoice_id
    ON invoice_payments (connection_id, external_invoice_id)
    WHERE external_invoice_id IS NOT NULL;

CREATE INDEX idx_invoice_payments_due
    ON invoice_payments (next_reconcile_at, payment_id)
    WHERE next_reconcile_at IS NOT NULL;

CREATE TRIGGER invoice_payments_identity_immutable
    BEFORE UPDATE ON invoice_payments
    FOR EACH ROW
    EXECUTE FUNCTION protect_invoice_payment_identity();

CREATE TRIGGER invoice_payments_parent_valid
    BEFORE INSERT OR UPDATE ON invoice_payments
    FOR EACH ROW
    EXECUTE FUNCTION validate_invoice_payment_parent();

CREATE TRIGGER invoice_payments_delete_rejected
    BEFORE DELETE ON invoice_payments
    FOR EACH ROW
    EXECUTE FUNCTION reject_invoice_payment_delete();

CREATE TABLE invoice_evidence (
    id UUID PRIMARY KEY,
    payment_id UUID NOT NULL,
    connection_id TEXT NOT NULL,
    financial_key TEXT NOT NULL CHECK (btrim(financial_key) <> ''),
    normalized_hash BYTEA NOT NULL CHECK (octet_length(normalized_hash) = 32),
    evidence_mode TEXT NOT NULL CHECK (evidence_mode IN ('invoice_confirmation', 'bank_credits')),
    amount_minor BIGINT NOT NULL CHECK (amount_minor BETWEEN 1 AND 9007199254740991),
    currency TEXT NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    eligible BOOLEAN NOT NULL,
    recorded_at TIMESTAMPTZ NOT NULL,
    provider_occurred_at TIMESTAMPTZ,
    verification_details JSONB NOT NULL DEFAULT '{}'::JSONB
        CHECK (jsonb_typeof(verification_details) = 'object'),
    CONSTRAINT invoice_evidence_invoice_scope_fk
        FOREIGN KEY (payment_id, connection_id, evidence_mode)
        REFERENCES invoice_payments (payment_id, connection_id, evidence_mode)
        ON DELETE RESTRICT,
    CONSTRAINT invoice_evidence_connection_financial_key_unique UNIQUE (connection_id, financial_key)
);

CREATE INDEX idx_invoice_evidence_payment_id
    ON invoice_evidence (payment_id);

CREATE UNIQUE INDEX idx_invoice_evidence_invoice_confirmation_once
    ON invoice_evidence (payment_id)
    WHERE evidence_mode = 'invoice_confirmation';

CREATE TRIGGER invoice_evidence_immutable
    BEFORE UPDATE OR DELETE ON invoice_evidence
    FOR EACH ROW
    EXECUTE FUNCTION reject_invoice_evidence_mutation();

-- +goose Down
-- Run only against an isolated verification database.
LOCK TABLE invoice_evidence, invoice_payments, payments IN ACCESS EXCLUSIVE MODE;
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM invoice_evidence)
       OR EXISTS (SELECT 1 FROM invoice_payments)
       OR EXISTS (
            SELECT 1
            FROM payments
            WHERE provider_name LIKE 'invoice:%'
       ) THEN
        RAISE EXCEPTION 'refusing to drop invoice payment data'
            USING ERRCODE = '55000';
    END IF;
END;
$$;
-- +goose StatementEnd

DROP TABLE invoice_evidence;
DROP TABLE invoice_payments;
DROP FUNCTION reject_invoice_payment_delete();
DROP FUNCTION validate_invoice_payment_parent();
DROP FUNCTION reject_invoice_evidence_mutation();
DROP FUNCTION protect_invoice_payment_identity();
DROP FUNCTION invoice_review_reasons_are_valid(TEXT[]);
