package bootstrap

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/Seraf-seraf/payment/adapter/httpapi"
	"github.com/Seraf-seraf/payment/adapter/notification/httpcallback"
	cloudpaymentsprovider "github.com/Seraf-seraf/payment/adapter/provider/cloudpayments"
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
	invoiceRepository := postgresstorage.NewInvoiceRepository(pool)
	webhookRepository := postgresstorage.NewWebhookRepository(pool)
	outboxRepository := postgresstorage.NewOutboxRepository(pool)
	txManager := postgresstorage.NewTransactionManager(pool, cfg.Security.EncryptionKey)
	providers, err := buildProviderRegistry(cfg, paymentRepository)
	if err != nil {
		return nil, err
	}

	merchantService := merchantservice.NewService(merchantRepository)
	paymentService := paymentservice.NewService(paymentRepository, providers, func() time.Time {
		return time.Now().UTC()
	})

	webhookService := webhookservice.NewService(providers, paymentRepository, webhookRepository, outboxRepository, txManager)

	var server *http.Server
	mode := cfg.App.Mode
	if mode == "" {
		mode = "all"
	}
	if mode != "api" && mode != "worker" && mode != "all" {
		return nil, errors.New("app mode must be api, worker or all")
	}

	invoices, err := buildInvoices(ctx, cfg, mode, merchantRepository, invoiceRepository, txManager, func() time.Time { return time.Now().UTC() }, log)
	if err != nil {
		return nil, err
	}

	if mode == "api" || mode == "all" {
		router := httpapi.NewRouter(log, merchantService, paymentService, invoices.service, webhookService, cfg.Security.HMACMaxSkew, httpapi.InvoiceWebhookDependencies{Notifications: invoices.notifications, Decoders: invoices.registry})
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

	if invoices.worker != nil {
		invoices.worker.Start(ctx)
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

	var stopInvoiceWorker func(context.Context) error
	if invoices.worker != nil {
		stopInvoiceWorker = invoices.worker.Stop
	}
	return func(shutdownCtx context.Context) error {
		return shutdownRuntime(shutdownCtx, stopInvoiceWorker, worker, server, closers)
	}, nil
}

func shutdownRuntime(
	ctx context.Context,
	stopInvoiceWorker func(context.Context) error,
	outboxWorker ports.OutboxWorker,
	server *http.Server,
	closers []func(),
) error {
	var shutdownErr error
	invoiceStopped := true
	if stopInvoiceWorker != nil {
		err := stopInvoiceWorker(ctx)
		invoiceStopped = err == nil
		shutdownErr = errors.Join(shutdownErr, err)
	}
	if outboxWorker != nil {
		shutdownErr = errors.Join(shutdownErr, outboxWorker.Stop(ctx))
	}
	if server != nil {
		shutdownErr = errors.Join(shutdownErr, server.Shutdown(ctx))
	}
	if invoiceStopped {
		for _, close := range closers {
			close()
		}
	}
	return shutdownErr
}

func buildProviderRegistry(cfg config.Config, payments ports.PaymentRepository) (*ProviderRegistry, error) {
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
	if cfg.Providers.CloudPayments.Enabled {
		p := cfg.Providers.CloudPayments
		provider, err := cloudpaymentsprovider.New(cloudpaymentsprovider.Options{APIURL: p.APIURL, PublicID: p.PublicID, APISecret: p.APISecret, TestMode: p.TestMode, Timeout: p.Timeout, Payments: payments})
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
