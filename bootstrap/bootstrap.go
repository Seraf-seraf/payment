package bootstrap

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/Seraf-seraf/payment/adapter/httpapi"
	mockprovider "github.com/Seraf-seraf/payment/adapter/provider/mock"
	tbankprovider "github.com/Seraf-seraf/payment/adapter/provider/tbank"
	postgresstorage "github.com/Seraf-seraf/payment/adapter/storage/postgres"
	"github.com/Seraf-seraf/payment/pkg/config"
	"github.com/Seraf-seraf/payment/pkg/db"
	"github.com/Seraf-seraf/payment/pkg/logger"
	"github.com/Seraf-seraf/payment/ports"
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
	providers, err := buildProviderRegistry(cfg)
	if err != nil {
		return nil, err
	}
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

func buildProviderRegistry(cfg config.Config) (*ProviderRegistry, error) {
	providers := make([]ports.PaymentProvider, 0, 2)
	if cfg.Providers.Mock.Enabled {
		providers = append(providers, mockprovider.New(cfg.Providers.Mock.WebhookSecret))
	}
	if cfg.Providers.TBank.Enabled {
		provider, err := tbankprovider.New(tbankprovider.Options{
			APIURL:          cfg.Providers.TBank.APIURL,
			TerminalKey:     cfg.Providers.TBank.TerminalKey,
			Password:        cfg.Providers.TBank.Password,
			NotificationURL: cfg.Providers.TBank.NotificationURL,
		})
		if err != nil {
			return nil, err
		}
		providers = append(providers, provider)
	}
	if len(providers) == 0 {
		return nil, errors.New("at least one provider is required")
	}
	return NewProviderRegistry(providers...), nil
}
