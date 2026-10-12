package bootstrap

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"time"

	mockinvoice "github.com/Seraf-seraf/payment/adapter/provider/mockinvoice"
	invoicedomain "github.com/Seraf-seraf/payment/domain/invoice"
	"github.com/Seraf-seraf/payment/pkg/config"
	"github.com/Seraf-seraf/payment/ports"
	invoiceservice "github.com/Seraf-seraf/payment/service/invoice"
	"github.com/google/uuid"
)

type invoiceComponents struct {
	registry      *InvoiceProviderRegistry
	service       ports.InvoiceUseCase
	notifications ports.InvoiceNotificationUseCase
	worker        *invoiceservice.Worker
}

func buildInvoices(ctx context.Context, cfg config.Config, mode string, merchants ports.MerchantRepository, invoices ports.InvoiceRepository, tx ports.TransactionManager, now func() time.Time, log *slog.Logger) (invoiceComponents, error) {
	var components invoiceComponents
	registry, err := buildInvoiceProviderRegistry(ctx, cfg, merchants)
	if err != nil {
		return components, err
	}
	components.registry = registry
	if !cfg.Invoices.Enabled {
		return components, nil
	}
	if invoices == nil || tx == nil || now == nil {
		return components, fmt.Errorf("invoice dependencies are required")
	}
	components.service = invoiceservice.NewService(invoices, registry, tx, now)
	components.notifications = invoiceservice.NewNotificationService(tx, now)
	if cfg.Invoices.WorkerEnabled && (mode == "worker" || mode == "all") {
		workerLog := log
		if workerLog != nil {
			workerLog = workerLog.With(slog.String("component", "invoice_worker"))
		}
		components.worker, err = invoiceservice.NewWorker(tx, registry, now, invoiceservice.WorkerConfig{
			PollInterval: cfg.Invoices.PollInterval, BatchSize: cfg.Invoices.CommandBatchSize, MaxAttempts: cfg.Invoices.MaxAttempts,
			CommandLease: cfg.Invoices.CommandLease, ProviderTimeout: cfg.Invoices.ProviderTimeout,
			ReconcileInterval: cfg.Invoices.ReconcileInterval, ExpiredReconcileInterval: cfg.Invoices.ExpiredReconcileInterval,
		}, workerLog)
		if err != nil {
			return invoiceComponents{}, err
		}
	}
	return components, nil
}

type InvoiceProviderRegistry struct {
	connections map[uuid.UUID]invoicedomain.Connection
	providers   map[string]ports.InvoiceProvider
	byID        map[string]invoicedomain.Connection
	decoders    map[string]ports.InvoiceNotificationDecoder
}

var _ ports.InvoiceProviderRegistry = (*InvoiceProviderRegistry)(nil)

func buildInvoiceProviderRegistry(ctx context.Context, cfg config.Config, merchants ports.MerchantRepository) (*InvoiceProviderRegistry, error) {
	registry := &InvoiceProviderRegistry{connections: make(map[uuid.UUID]invoicedomain.Connection, len(cfg.Invoices.Connections)), providers: map[string]ports.InvoiceProvider{}, byID: map[string]invoicedomain.Connection{}, decoders: map[string]ports.InvoiceNotificationDecoder{}}

	if !cfg.Invoices.Enabled {
		return registry, nil
	}
	if err := cfg.Invoices.Validate(); err != nil {
		return nil, err
	}
	if merchants == nil {
		return nil, fmt.Errorf("invoice merchant repository is required")
	}
	if len(cfg.Invoices.Connections) == 0 {
		return nil, fmt.Errorf("enabled invoices require a connection")
	}
	connectionIDs := make(map[string]struct{})
	for _, configured := range cfg.Invoices.Connections {
		id, err := uuid.Parse(configured.ID)
		if err != nil || id == uuid.Nil {
			return nil, fmt.Errorf("invalid invoice connection UUID")
		}
		configured.ID = id.String()
		merchantID, err := uuid.Parse(configured.MerchantID)
		if err != nil || merchantID == uuid.Nil {
			return nil, fmt.Errorf("invalid invoice merchant UUID")
		}
		if _, exists := connectionIDs[configured.ID]; exists {
			return nil, fmt.Errorf("duplicate invoice connection id")
		}
		if _, exists := registry.connections[merchantID]; exists {
			return nil, fmt.Errorf("multiple invoice connections for merchant")
		}
		merchant, err := merchants.FindByID(ctx, merchantID)
		if err != nil {
			return nil, fmt.Errorf("invoice merchant unavailable: %w", err)
		}
		if merchant.ID != merchantID {
			return nil, fmt.Errorf("invoice merchant does not match connection")
		}
		if configured.ProviderKind != "mock_invoice" {
			return nil, fmt.Errorf("unknown invoice provider kind")
		}
		if cfg.App.Env != "local" && cfg.App.Env != "test" {
			return nil, fmt.Errorf("mock_invoice is available only in local and test environments")
		}
		if configured.EvidenceMode != "invoice_confirmation" && configured.EvidenceMode != "bank_credits" {
			return nil, fmt.Errorf("invalid invoice evidence mode")
		}
		if configured.CreationReplay != "provider_idempotent" {
			return nil, fmt.Errorf("mock_invoice requires provider_idempotent creation replay")
		}
		if !invoiceSecretEnvName.MatchString(configured.WebhookSecretEnv) {
			return nil, fmt.Errorf("invalid invoice webhook secret environment variable name")
		}
		secret, ok := os.LookupEnv(configured.WebhookSecretEnv)
		if !ok || strings.TrimSpace(secret) == "" {
			return nil, fmt.Errorf("invoice webhook secret environment variable is empty")
		}
		b := configured.Beneficiary
		if strings.TrimSpace(b.Name) == "" || strings.TrimSpace(b.BankName) == "" || !digits(b.INN, 10) || !digits(b.KPP, 9) || !digits(b.Account, 20) || !digits(b.BankBIK, 9) || !digits(b.CorrespondentAccount, 20) {
			return nil, fmt.Errorf("invalid invoice beneficiary")
		}
		connection := invoicedomain.Connection{ID: configured.ID, MerchantID: merchantID, ProviderKind: configured.ProviderKind, EvidenceMode: configured.EvidenceMode,
			Beneficiary: invoicedomain.Beneficiary{Name: b.Name, INN: b.INN, KPP: b.KPP, Account: b.Account, BankBIK: b.BankBIK, BankName: b.BankName, CorrespondentAccount: b.CorrespondentAccount}}
		provider := mockinvoice.New()
		connection.Capabilities = provider.Capabilities()
		decoder, err := mockinvoice.NewNotificationDecoder(secret, configured.EvidenceMode, configured.OperationIDStrategy, configured.OperationAliases)
		if err != nil {
			return nil, fmt.Errorf("invalid invoice notification configuration")
		}
		registry.connections[merchantID] = connection
		registry.byID[configured.ID] = connection
		registry.providers[configured.ID] = provider
		registry.decoders[configured.ID] = decoder
		connectionIDs[configured.ID] = struct{}{}
	}

	return registry, nil
}

func (r *InvoiceProviderRegistry) ForMerchant(merchantID uuid.UUID) (invoicedomain.Connection, bool) {
	connection, ok := r.connections[merchantID]
	return connection, ok
}

func (r *InvoiceProviderRegistry) ForConnection(id string) (invoicedomain.Connection, ports.InvoiceProvider, bool) {
	c, ok := r.byID[id]
	return c, r.providers[id], ok
}

func (r *InvoiceProviderRegistry) DecoderForConnection(id string) (invoicedomain.Connection, ports.InvoiceNotificationDecoder, bool) {
	c, ok := r.byID[id]
	return c, r.decoders[id], ok
}

var invoiceSecretEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func digits(value string, size int) bool {
	if len(value) != size {
		return false
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
