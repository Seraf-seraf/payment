package postgres

import (
	"context"

	merchantdomain "github.com/Seraf-seraf/payment/domain/merchant"
	"github.com/Seraf-seraf/payment/pkg/crypto"
	"github.com/Seraf-seraf/payment/ports"
	merchantservice "github.com/Seraf-seraf/payment/service/merchant"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MerchantRepository реализует хранилище мерчантов в PostgreSQL.
type MerchantRepository struct {
	db            queryer
	encryptionKey string
}

var _ ports.MerchantRepository = (*MerchantRepository)(nil)

// NewMerchantRepository создает PostgreSQL-репозиторий мерчантов.
func NewMerchantRepository(pool *pgxpool.Pool, encryptionKey string) *MerchantRepository {
	return &MerchantRepository{db: pool, encryptionKey: encryptionKey}
}

func newMerchantRepository(db queryer, encryptionKey string) *MerchantRepository {
	return &MerchantRepository{db: db, encryptionKey: encryptionKey}
}

// FindByAPIKeyHash возвращает мерчанта по hash(API key).
func (r *MerchantRepository) FindByAPIKeyHash(ctx context.Context, apiKeyHash string) (merchantdomain.Merchant, error) {
	const query = `
SELECT id,
       name,
       api_key_hash,
       shared_secret_encrypted,
       callback_url,
       COALESCE(success_url, ''),
       COALESCE(fail_url, ''),
       provider_name,
       is_active
FROM merchants
WHERE api_key_hash = $1`
	return r.queryMerchant(ctx, query, apiKeyHash)
}

// FindByID возвращает мерчанта по идентификатору.
func (r *MerchantRepository) FindByID(ctx context.Context, id uuid.UUID) (merchantdomain.Merchant, error) {
	const query = `
SELECT id,
       name,
       api_key_hash,
       shared_secret_encrypted,
       callback_url,
       COALESCE(success_url, ''),
       COALESCE(fail_url, ''),
       provider_name,
       is_active
FROM merchants
WHERE id = $1`
	return r.queryMerchant(ctx, query, id)
}

func (r *MerchantRepository) queryMerchant(ctx context.Context, query string, args ...any) (merchantdomain.Merchant, error) {
	var merchant merchantdomain.Merchant
	err := r.db.QueryRow(ctx, query, args...).Scan(
		&merchant.ID,
		&merchant.Name,
		&merchant.APIKeyHash,
		&merchant.SharedSecret,
		&merchant.CallbackURL,
		&merchant.SuccessURL,
		&merchant.FailURL,
		&merchant.ProviderName,
		&merchant.IsActive,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return merchantdomain.Merchant{}, merchantservice.ErrNotFound
		}
		return merchantdomain.Merchant{}, err
	}
	sharedSecret, err := crypto.DecryptSecret(r.encryptionKey, merchant.SharedSecret)
	if err != nil {
		return merchantdomain.Merchant{}, err
	}
	merchant.SharedSecret = sharedSecret
	return merchant, nil
}
