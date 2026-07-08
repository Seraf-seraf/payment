package db

import (
	"context"
	"time"

	"github.com/Seraf-seraf/payment/pkg/config"
	"github.com/jackc/pgx/v5/pgxpool"
)

func OpenPostgres(ctx context.Context, cfg config.Postgres) (*pgxpool.Pool, error) {
	poolConfig, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, err
	}
	if cfg.MaxOpenConns > 0 {
		poolConfig.MaxConns = cfg.MaxOpenConns
	}
	if cfg.MaxIdleConns > 0 {
		poolConfig.MinConns = cfg.MaxIdleConns
	}
	if cfg.ConnMaxLifetime > 0 {
		poolConfig.MaxConnLifetime = cfg.ConnMaxLifetime
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, err
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}
