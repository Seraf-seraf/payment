package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	paymentdomain "github.com/Seraf-seraf/payment/domain/payment"
	"github.com/Seraf-seraf/payment/ports"
	paymentservice "github.com/Seraf-seraf/payment/service/payment"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PaymentRepository реализует хранилище платежей в PostgreSQL.
type PaymentRepository struct {
	db queryer
}

var _ ports.PaymentRepository = (*PaymentRepository)(nil)

// NewPaymentRepository создает PostgreSQL-репозиторий платежей.
func NewPaymentRepository(pool *pgxpool.Pool) *PaymentRepository {
	return &PaymentRepository{db: pool}
}

func newPaymentRepository(db queryer) *PaymentRepository {
	return &PaymentRepository{db: db}
}

// Create сохраняет новый платеж.
func (r *PaymentRepository) Create(ctx context.Context, payment paymentdomain.Payment) error {
	metadata, err := json.Marshal(payment.Metadata)
	if err != nil {
		return err
	}

	const query = `
INSERT INTO payments (
    id,
    merchant_id,
    provider_name,
    provider_payment_id,
    merchant_order_id,
    idempotency_key,
    amount_minor,
    currency,
    status,
    payment_url,
    metadata,
    created_at,
    updated_at,
    paid_at,
    canceled_at,
    refunded_at
) VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, $7, $8, $9, NULLIF($10, ''), $11, $12, $13, $14, $15, $16)`

	_, err = r.db.Exec(ctx, query,
		payment.ID,
		payment.MerchantID,
		payment.ProviderName,
		payment.ProviderPaymentID,
		payment.MerchantOrderID,
		payment.IdempotencyKey,
		payment.AmountMinor,
		payment.Currency,
		payment.Status,
		payment.PaymentURL,
		metadata,
		payment.CreatedAt,
		payment.UpdatedAt,
		payment.PaidAt,
		payment.CanceledAt,
		payment.RefundedAt,
	)
	if isUniqueViolation(err, "payments_merchant_id_idempotency_key_key") {
		return paymentservice.ErrAlreadyExists
	}
	return err
}

// FindByID возвращает платеж по идентификатору.
func (r *PaymentRepository) FindByID(ctx context.Context, id uuid.UUID) (paymentdomain.Payment, error) {
	const query = `
SELECT id,
       merchant_id,
       provider_name,
       COALESCE(provider_payment_id, ''),
       merchant_order_id,
       idempotency_key,
       amount_minor,
       currency,
       status,
       COALESCE(payment_url, ''),
       metadata,
       created_at,
       updated_at,
       paid_at,
       canceled_at,
       refunded_at
FROM payments
WHERE id = $1`
	return r.queryPayment(ctx, query, id)
}

// FindByMerchantAndIdempotencyKey возвращает платеж по мерчанту и идемпотентному ключу.
func (r *PaymentRepository) FindByMerchantAndIdempotencyKey(ctx context.Context, merchantID uuid.UUID, key string) (paymentdomain.Payment, error) {
	const query = `
SELECT id,
       merchant_id,
       provider_name,
       COALESCE(provider_payment_id, ''),
       merchant_order_id,
       idempotency_key,
       amount_minor,
       currency,
       status,
       COALESCE(payment_url, ''),
       metadata,
       created_at,
       updated_at,
       paid_at,
       canceled_at,
       refunded_at
FROM payments
WHERE merchant_id = $1 AND idempotency_key = $2`
	return r.queryPayment(ctx, query, merchantID, key)
}

// FindByProviderPaymentID возвращает платеж по идентификатору провайдера.
func (r *PaymentRepository) FindByProviderPaymentID(ctx context.Context, providerName, providerPaymentID string) (paymentdomain.Payment, error) {
	const query = `
SELECT id,
       merchant_id,
       provider_name,
       COALESCE(provider_payment_id, ''),
       merchant_order_id,
       idempotency_key,
       amount_minor,
       currency,
       status,
       COALESCE(payment_url, ''),
       metadata,
       created_at,
       updated_at,
       paid_at,
       canceled_at,
       refunded_at
FROM payments
WHERE provider_name = $1 AND provider_payment_id = $2`
	return r.queryPayment(ctx, query, providerName, providerPaymentID)
}

// UpdateProviderData сохраняет данные провайдера для платежа.
func (r *PaymentRepository) UpdateProviderData(ctx context.Context, id uuid.UUID, providerPaymentID, paymentURL string) error {
	const query = `
UPDATE payments
SET provider_payment_id = $2,
    payment_url = $3,
    updated_at = now()
WHERE id = $1`

	tag, err := r.db.Exec(ctx, query, id, providerPaymentID, paymentURL)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return paymentservice.ErrNotFound
	}
	return nil
}

// UpdateProviderDataAndStatus атомарно сохраняет данные провайдера и меняет статус при допустимом текущем статусе.
func (r *PaymentRepository) UpdateProviderDataAndStatus(
	ctx context.Context,
	id uuid.UUID,
	providerPaymentID string,
	paymentURL string,
	status paymentdomain.Status,
	allowedCurrent []paymentdomain.Status,
) error {
	const query = `
UPDATE payments
SET provider_payment_id = $2,
    payment_url = $3,
    status = $4,
    updated_at = now()
WHERE id = $1
  AND status = ANY($5::text[])`

	tag, err := r.db.Exec(ctx, query, id, providerPaymentID, paymentURL, status, statusStrings(allowedCurrent))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return paymentservice.ErrInvalidStatusTransition
	}
	return nil
}

// UpdateStatus меняет статус платежа без проверки допустимости перехода.
func (r *PaymentRepository) UpdateStatus(ctx context.Context, id uuid.UUID, status paymentdomain.Status) error {
	const query = `
UPDATE payments
SET status = $2,
    updated_at = now(),
    paid_at = CASE WHEN $2 = 'succeeded' THEN now() ELSE paid_at END,
    canceled_at = CASE WHEN $2 = 'canceled' THEN now() ELSE canceled_at END,
    refunded_at = CASE WHEN $2 = 'refunded' THEN now() ELSE refunded_at END
WHERE id = $1`

	tag, err := r.db.Exec(ctx, query, id, status)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return paymentservice.ErrNotFound
	}
	return nil
}

// UpdateStatusIfAllowed меняет статус платежа только из разрешенных текущих статусов.
func (r *PaymentRepository) UpdateStatusIfAllowed(ctx context.Context, id uuid.UUID, status paymentdomain.Status, allowedCurrent []paymentdomain.Status) error {
	const query = `
UPDATE payments
SET status = $2,
    updated_at = now(),
    paid_at = CASE WHEN $2 = 'succeeded' THEN now() ELSE paid_at END,
    canceled_at = CASE WHEN $2 = 'canceled' THEN now() ELSE canceled_at END,
    refunded_at = CASE WHEN $2 = 'refunded' THEN now() ELSE refunded_at END
WHERE id = $1
  AND status = ANY($3::text[])`

	tag, err := r.db.Exec(ctx, query, id, status, statusStrings(allowedCurrent))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return paymentservice.ErrInvalidStatusTransition
	}
	return nil
}

func (r *PaymentRepository) queryPayment(ctx context.Context, query string, args ...any) (paymentdomain.Payment, error) {
	var payment paymentdomain.Payment
	var metadata []byte
	var paidAt sql.NullTime
	var canceledAt sql.NullTime
	var refundedAt sql.NullTime
	err := r.db.QueryRow(ctx, query, args...).Scan(
		&payment.ID,
		&payment.MerchantID,
		&payment.ProviderName,
		&payment.ProviderPaymentID,
		&payment.MerchantOrderID,
		&payment.IdempotencyKey,
		&payment.AmountMinor,
		&payment.Currency,
		&payment.Status,
		&payment.PaymentURL,
		&metadata,
		&payment.CreatedAt,
		&payment.UpdatedAt,
		&paidAt,
		&canceledAt,
		&refundedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return paymentdomain.Payment{}, paymentservice.ErrNotFound
		}
		return paymentdomain.Payment{}, err
	}
	if len(metadata) > 0 {
		if err := json.Unmarshal(metadata, &payment.Metadata); err != nil {
			return paymentdomain.Payment{}, err
		}
	}
	payment.PaidAt = nullTimePtr(paidAt)
	payment.CanceledAt = nullTimePtr(canceledAt)
	payment.RefundedAt = nullTimePtr(refundedAt)
	return payment, nil
}

func nullTimePtr(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	return &value.Time
}

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}

func statusStrings(statuses []paymentdomain.Status) []string {
	values := make([]string, 0, len(statuses))
	for _, status := range statuses {
		values = append(values, string(status))
	}
	return values
}
