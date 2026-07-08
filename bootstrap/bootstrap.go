package bootstrap

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/Seraf-seraf/payment/adapter/httpapi"
	mockprovider "github.com/Seraf-seraf/payment/adapter/provider/mock"
	postgresstorage "github.com/Seraf-seraf/payment/adapter/storage/postgres"
	"github.com/Seraf-seraf/payment/pkg/config"
	"github.com/Seraf-seraf/payment/pkg/db"
	"github.com/Seraf-seraf/payment/pkg/logger"
	merchantservice "github.com/Seraf-seraf/payment/service/merchant"
	paymentservice "github.com/Seraf-seraf/payment/service/payment"
	webhookservice "github.com/Seraf-seraf/payment/service/webhook"
)

func Run(ctx context.Context, cfg config.Config) (func(context.Context) error, error) {
	log := logger.New(cfg.App.Name, cfg.App.Env)
	var closers []func()
	if cfg.Postgres.DSN == "" {
		return nil, errors.New("postgres dsn is required")
	}
	pool, err := db.OpenPostgres(ctx, cfg.Postgres)
	if err != nil {
		return nil, err
	}
	closers = append(closers, pool.Close)
	log.Info("postgres connected")

	merchantRepository := postgresstorage.NewMerchantRepository(pool)
	paymentRepository := postgresstorage.NewPaymentRepository(pool)
	webhookRepository := postgresstorage.NewWebhookRepository(pool)
	providers := NewProviderRegistry(mockprovider.New(cfg.Providers.Mock.WebhookSecret))
	merchantService := merchantservice.NewService(merchantRepository)
	paymentService := paymentservice.NewService(paymentRepository, providers, func() time.Time {
		return time.Now().UTC()
	})
	webhookService := webhookservice.NewService(providers, paymentRepository, webhookRepository)

	router := httpapi.NewRouter(log, merchantService, paymentService, webhookService, cfg.Security.HMACMaxSkew)
	server := &http.Server{
		Addr:              cfg.HTTP.Addr,
		Handler:           router,
		ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
	}

	go func() {
		log.Info("http server started", slog.String("addr", cfg.HTTP.Addr))
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server failed", slog.Any("error", err))
		}
	}()

	return func(shutdownCtx context.Context) error {
		err := server.Shutdown(shutdownCtx)
		for _, close := range closers {
			close()
		}
		return err
	}, nil
}
