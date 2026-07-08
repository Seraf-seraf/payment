package postgres

import (
	"context"

	merchantdomain "github.com/Seraf-seraf/payment/domain/merchant"
	"github.com/Seraf-seraf/payment/ports"
	merchantservice "github.com/Seraf-seraf/payment/service/merchant"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type MerchantRepository struct {
	pool *pgxpool.Pool
}

var _ ports.MerchantRepository = (*MerchantRepository)(nil)

func NewMerchantRepository(pool *pgxpool.Pool) *MerchantRepository {
	return &MerchantRepository{pool: pool}
}

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

	var merchant merchantdomain.Merchant
	err := r.pool.QueryRow(ctx, query, apiKeyHash).Scan(
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
	return merchant, nil
}
