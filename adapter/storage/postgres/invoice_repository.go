package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	paymentdomain "github.com/Seraf-seraf/payment/domain/payment"
	"time"

	invoicedomain "github.com/Seraf-seraf/payment/domain/invoice"
	"github.com/Seraf-seraf/payment/ports"
	paymentservice "github.com/Seraf-seraf/payment/service/payment"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type InvoiceRepository struct {
	db queryer
}

var _ ports.InvoiceRepository = (*InvoiceRepository)(nil)

func NewInvoiceRepository(pool *pgxpool.Pool) *InvoiceRepository {
	return &InvoiceRepository{db: pool}
}

func newInvoiceRepository(db queryer) *InvoiceRepository {
	return &InvoiceRepository{db: db}
}

func (r *InvoiceRepository) FindByMerchantAndRequestID(ctx context.Context, merchantID, requestID uuid.UUID) (invoicedomain.Record, error) {
	const query = `SELECT ` + invoiceProjection + ` FROM invoice_payments WHERE merchant_id = $1 AND request_id = $2`
	return r.scanRecord(ctx, query, merchantID, requestID)
}

func (r *InvoiceRepository) Create(ctx context.Context, record invoicedomain.Record) (invoicedomain.Record, error) {
	requestSnapshot, err := json.Marshal(record.Request)
	if err != nil {
		return invoicedomain.Record{}, err
	}
	connectionSnapshot, err := json.Marshal(record.Connection)
	if err != nil {
		return invoicedomain.Record{}, err
	}
	const query = `
INSERT INTO invoice_payments (
    payment_id, merchant_id, request_id, request_hash, request_snapshot,
    connection_id, provider_kind, connection_snapshot, reference, evidence_mode,
    issuance_state, funding_state, credited_minor, review_reasons, revision,
    accept_until, created_at, updated_at
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,0,'{}',$13,$14,$15,$15)
RETURNING invoice_number`
	err = r.db.QueryRow(ctx, query,
		record.PaymentID, record.MerchantID, record.Request.RequestID, record.RequestHash[:], requestSnapshot,
		record.Connection.ID, record.Connection.ProviderKind, connectionSnapshot, record.Reference,
		record.EvidenceMode, record.IssuanceState, record.FundingState, record.Revision,
		record.Request.AcceptUntil, record.CreatedAt,
	).Scan(&record.InvoiceNumber)
	if err != nil {
		return invoicedomain.Record{}, err
	}
	return record, nil
}

func (r *InvoiceRepository) scanRecord(ctx context.Context, query string, args ...any) (invoicedomain.Record, error) {
	var record invoicedomain.Record
	var requestHash []byte
	var requestSnapshot []byte
	var connectionSnapshot []byte
	var artifact []byte
	err := r.db.QueryRow(ctx, query, args...).Scan(
		&record.PaymentID, &record.MerchantID, &requestHash, &requestSnapshot, &connectionSnapshot,
		&record.InvoiceNumber, &record.Reference, &record.EvidenceMode, &record.IssuanceState, &record.FundingState,
		&record.CreditedMinor, &record.ReviewReasons, &record.Revision, &record.PaymentConfirmedAt, &record.ConfirmationRecordedAt,
		&record.ExternalInvoiceID, &artifact, &record.ProviderExpiresAt, &record.DispatchedAt, &record.NextReconcileAt, &record.CreatedAt, &record.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return invoicedomain.Record{}, paymentservice.ErrNotFound
		}
		return invoicedomain.Record{}, err
	}
	if len(requestHash) != sha256.Size {
		return invoicedomain.Record{}, errors.New("invalid stored invoice request hash")
	}
	copy(record.RequestHash[:], requestHash)
	if err := json.Unmarshal(requestSnapshot, &record.Request); err != nil {
		return invoicedomain.Record{}, err
	}
	if err := json.Unmarshal(connectionSnapshot, &record.Connection); err != nil {
		return invoicedomain.Record{}, err
	}
	if len(artifact) > 0 {
		if err := json.Unmarshal(artifact, &record.Artifact); err != nil {
			return invoicedomain.Record{}, err
		}
	}
	return record, nil
}

type InvoiceCommandRepository struct {
	db queryer
}

var _ ports.InvoiceCommandRepository = (*InvoiceCommandRepository)(nil)

func NewInvoiceCommandRepository(pool *pgxpool.Pool) *InvoiceCommandRepository {
	return &InvoiceCommandRepository{db: pool}
}

func newInvoiceCommandRepository(db queryer) *InvoiceCommandRepository {
	return &InvoiceCommandRepository{db: db}
}

func (r *InvoiceCommandRepository) Create(ctx context.Context, command ports.InvoiceCommand) error {
	_, err := r.db.Exec(ctx, `INSERT INTO invoice_commands(id,command_type,payment_id,inbox_id,status,next_attempt_at,created_at,updated_at)
 VALUES($1,$2,NULLIF($3::uuid,'00000000-0000-0000-0000-000000000000'),NULLIF($4::uuid,'00000000-0000-0000-0000-000000000000'),$5,$6,$7,$8)
 ON CONFLICT DO NOTHING`, command.ID, command.CommandType, command.PaymentID, command.InboxID, command.Status, command.NextAttemptAt, command.CreatedAt, command.UpdatedAt)
	return err
}

const invoiceProjection = `payment_id,merchant_id,request_hash,request_snapshot,connection_snapshot,
 invoice_number,reference,evidence_mode,issuance_state,funding_state,credited_minor,review_reasons,revision,
 payment_confirmed_at,confirmation_recorded_at,COALESCE(external_invoice_id,''),artifact,provider_expires_at,
 dispatched_at,next_reconcile_at,created_at,updated_at`

func (r *InvoiceRepository) Lock(ctx context.Context, id uuid.UUID) (invoicedomain.Record, error) {
	return r.scanRecord(ctx, `SELECT `+invoiceProjection+` FROM invoice_payments WHERE payment_id=$1 FOR UPDATE`, id)
}

func (r *InvoiceRepository) LockByMerchantAndPaymentID(ctx context.Context, merchantID, paymentID uuid.UUID) (invoicedomain.Record, error) {
	return r.scanRecord(ctx, `SELECT `+invoiceProjection+` FROM invoice_payments WHERE merchant_id=$1 AND payment_id=$2 FOR UPDATE`, merchantID, paymentID)
}

func (r *InvoiceRepository) Save(ctx context.Context, v invoicedomain.Record) error {
	var artifact []byte
	var err error
	if v.Artifact != nil {
		artifact, err = json.Marshal(v.Artifact)
		if err != nil {
			return err
		}
	}
	_, err = r.db.Exec(ctx, `UPDATE invoice_payments SET external_invoice_id=NULLIF($2,''),artifact=$3,provider_expires_at=$4,
 dispatched_at=$5,next_reconcile_at=$6,issuance_state=$7,funding_state=$8,credited_minor=$9,review_reasons=$10,
 revision=$11,payment_confirmed_at=$12,confirmation_recorded_at=$13,updated_at=$14 WHERE payment_id=$1`, v.PaymentID, v.ExternalInvoiceID, artifact, v.ProviderExpiresAt, v.DispatchedAt, v.NextReconcileAt, v.IssuanceState, v.FundingState, v.CreditedMinor, v.ReviewReasons, v.Revision, v.PaymentConfirmedAt, v.ConfirmationRecordedAt, v.UpdatedAt)
	return err
}
func (r *InvoiceRepository) LockPayment(ctx context.Context, id uuid.UUID) (paymentdomain.Payment, error) {
	return (&PaymentRepository{db: r.db}).queryPayment(ctx, `SELECT `+paymentSelectProjection+` FROM payments WHERE id=$1 FOR UPDATE`, id)
}
func (r *InvoiceRepository) SavePayment(ctx context.Context, p paymentdomain.Payment, now time.Time) error {
	_, err := r.db.Exec(ctx, `UPDATE payments SET status=$2,updated_at=$3,paid_at=CASE WHEN $2='succeeded' THEN COALESCE(paid_at,$3) ELSE paid_at END,
 canceled_at=CASE WHEN $2='canceled' THEN COALESCE(canceled_at,$3) ELSE canceled_at END WHERE id=$1`, p.ID, p.Status, now)
	return err
}
func (r *InvoiceRepository) Match(ctx context.Context, connectionID, externalID, reference string) ([]uuid.UUID, error) {
	var id uuid.UUID
	var storedReference string
	var err error
	if externalID != "" {
		err = r.db.QueryRow(ctx, `SELECT payment_id,reference FROM invoice_payments WHERE connection_id=$1 AND external_invoice_id=$2`, connectionID, externalID).Scan(&id, &storedReference)
	} else if reference != "" {
		err = r.db.QueryRow(ctx, `SELECT payment_id,reference FROM invoice_payments WHERE connection_id=$1 AND reference=$2`, connectionID, reference).Scan(&id, &storedReference)
	} else {
		return nil, nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if reference != "" && reference != storedReference {
		return []uuid.UUID{id, uuid.Nil}, nil
	}
	return []uuid.UUID{id}, nil
}
func (r *InvoiceRepository) Due(ctx context.Context, now time.Time, limit int) ([]uuid.UUID, error) {
	rows, err := r.db.Query(ctx, `WITH due AS (SELECT payment_id FROM invoice_payments WHERE next_reconcile_at<=$1 ORDER BY next_reconcile_at,payment_id LIMIT $2) SELECT i.payment_id FROM invoice_payments i JOIN due ON due.payment_id=i.payment_id ORDER BY i.payment_id FOR UPDATE OF i SKIP LOCKED`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
func (r *InvoiceRepository) RecordEvidence(ctx context.Context, v invoicedomain.Evidence) (bool, bool, error) {
	details, err := json.Marshal(struct {
		Kind string `json:"verification_kind"`
	}{v.VerificationKind})
	if err != nil {
		return false, false, err
	}
	tag, err := r.db.Exec(ctx, `INSERT INTO invoice_evidence(id,payment_id,connection_id,financial_key,normalized_hash,evidence_mode,amount_minor,currency,eligible,recorded_at,provider_occurred_at,verification_details)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT DO NOTHING`, v.ID, v.PaymentID, v.ConnectionID, v.FinancialKey, v.NormalizedHash[:], v.EvidenceMode, v.AmountMinor, v.Currency, v.Eligible, v.RecordedAt, v.ProviderOccurredAt, details)
	if err != nil {
		return false, false, err
	}
	if tag.RowsAffected() == 1 {
		return true, false, nil
	}
	var id uuid.UUID
	var hash []byte
	err = r.db.QueryRow(ctx, `SELECT payment_id,normalized_hash FROM invoice_evidence WHERE (connection_id=$1 AND financial_key=$2) OR (payment_id=$3 AND evidence_mode='invoice_confirmation') ORDER BY recorded_at LIMIT 1`, v.ConnectionID, v.FinancialKey, v.PaymentID).Scan(&id, &hash)
	return false, id != v.PaymentID || string(hash) != string(v.NormalizedHash[:]), err
}

func (r *InvoiceCommandRepository) Claim(ctx context.Context, now time.Time, limit int, lease time.Duration) ([]ports.InvoiceCommand, error) {
	rows, err := r.db.Query(ctx, `WITH due AS (
 SELECT id FROM invoice_commands WHERE (status='pending' AND next_attempt_at<=$1) OR (status='processing' AND lease_expires_at<=$1)
 ORDER BY COALESCE(lease_expires_at,next_attempt_at),id LIMIT $2 FOR UPDATE SKIP LOCKED)
 UPDATE invoice_commands c SET status='processing',attempts=c.attempts+1,lease_token=gen_random_uuid(),lease_expires_at=$3,updated_at=$1
 FROM due WHERE c.id=due.id RETURNING c.id,c.command_type,COALESCE(c.payment_id,'00000000-0000-0000-0000-000000000000'::uuid),COALESCE(c.inbox_id,'00000000-0000-0000-0000-000000000000'::uuid),c.attempts,c.lease_token,c.lease_expires_at,c.next_attempt_at,c.created_at,c.updated_at`, now, limit, now.Add(lease))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var commands []ports.InvoiceCommand
	for rows.Next() {
		var c ports.InvoiceCommand
		if err := rows.Scan(&c.ID, &c.CommandType, &c.PaymentID, &c.InboxID, &c.Attempts, &c.LeaseToken, &c.LeaseExpiresAt, &c.NextAttemptAt, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		c.Status = "processing"
		commands = append(commands, c)
	}
	return commands, rows.Err()
}
func (r *InvoiceCommandRepository) Fence(ctx context.Context, c ports.InvoiceCommand) error {
	var id uuid.UUID
	err := r.db.QueryRow(ctx, `SELECT id FROM invoice_commands WHERE id=$1 AND status='processing' AND lease_token=$2 FOR UPDATE`, c.ID, c.LeaseToken).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ports.ErrLeaseLost
	}
	return err
}
func (r *InvoiceCommandRepository) Finish(ctx context.Context, c ports.InvoiceCommand, status string, next time.Time, errorCode string, now time.Time) error {
	tag, err := r.db.Exec(ctx, `UPDATE invoice_commands SET status=$3,next_attempt_at=$4,last_error_code=NULLIF($5,''),updated_at=$6,
 completed_at=CASE WHEN $3 IN ('completed','failed') THEN $6::timestamptz ELSE NULL END,lease_token=NULL,lease_expires_at=NULL
 WHERE id=$1 AND status='processing' AND lease_token=$2`, c.ID, c.LeaseToken, status, next, errorCode, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ports.ErrLeaseLost
	}
	return nil
}
