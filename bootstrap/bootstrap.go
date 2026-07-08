package bootstrap

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/Seraf-seraf/payment/adapter/httpapi"
	"github.com/Seraf-seraf/payment/adapter/notification/httpcallback"
	mockprovider "github.com/Seraf-seraf/payment/adapter/provider/mock"
	tbankprovider "github.com/Seraf-seraf/payment/adapter/provider/tbank"
	postgresstorage "github.com/Seraf-seraf/payment/adapter/storage/postgres"
	"github.com/Seraf-seraf/payment/pkg/config"
	"github.com/Seraf-seraf/payment/pkg/db"
	"github.com/Seraf-seraf/payment/pkg/logger"
	"github.com/Seraf-seraf/payment/ports"
	merchantservice "github.com/Seraf-seraf/payment/service/merchant"
	outboxservice "github.com/Seraf-seraf/payment/service/outbox"
	paymentservice "github.com/Seraf-seraf/payment/service/payment"
	webhookservice "github.com/Seraf-seraf/payment/service/webhook"
)

// Run собирает зависимости приложения, запускает выбранные компоненты и возвращает функцию остановки.
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

	merchantRepository := postgresstorage.NewMerchantRepository(pool, cfg.Security.EncryptionKey)
	paymentRepository := postgresstorage.NewPaymentRepository(pool)
	webhookRepository := postgresstorage.NewWebhookRepository(pool)
	outboxRepository := postgresstorage.NewOutboxRepository(pool)
	txManager := postgresstorage.NewTransactionManager(pool, cfg.Security.EncryptionKey)
	providers, err := buildProviderRegistry(cfg)
	if err != nil {
		return nil, err
	}
	merchantService := merchantservice.NewService(merchantRepository)
	paymentService := paymentservice.NewService(paymentRepository, providers, func() time.Time {
		return time.Now().UTC()
	}, txManager)
	webhookService := webhookservice.NewService(providers, paymentRepository, webhookRepository, outboxRepository, txManager)

	var server *http.Server
	mode := cfg.App.Mode
	if mode == "" {
		mode = "all"
	}
	if mode != "api" && mode != "worker" && mode != "all" {
		return nil, errors.New("app mode must be api, worker or all")
	}

	if mode == "api" || mode == "all" {
		router := httpapi.NewRouter(log, merchantService, paymentService, webhookService, cfg.Security.HMACMaxSkew)
		server = &http.Server{
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
	}

	var worker ports.OutboxWorker
	if cfg.Outbox.Enabled && (mode == "worker" || mode == "all") {
		callbackSender := httpcallback.New(cfg.Callback.Timeout)
		worker = outboxservice.NewWorker(log.With(slog.String("component", "outbox_worker")), outboxservice.Config{
			PollInterval: cfg.Outbox.PollInterval,
			BatchSize:    cfg.Outbox.BatchSize,
			MaxAttempts:  cfg.Outbox.MaxAttempts,
			WorkerCount:  cfg.Outbox.WorkerCount,
		}, merchantRepository, paymentRepository, outboxRepository, callbackSender, txManager, func() time.Time {
			return time.Now().UTC()
		})
		worker.Start(ctx)
		log.Info("outbox worker started", slog.Int("worker_count", cfg.Outbox.WorkerCount))
	}

	return func(shutdownCtx context.Context) error {
		var shutdownErr error
		if worker != nil {
			shutdownErr = errors.Join(shutdownErr, worker.Stop(shutdownCtx))
		}
		if server != nil {
			shutdownErr = errors.Join(shutdownErr, server.Shutdown(shutdownCtx))
		}
		for _, close := range closers {
			close()
		}
		return shutdownErr
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
