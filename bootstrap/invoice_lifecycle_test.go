package bootstrap

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/Seraf-seraf/payment/ports"
)

type invoiceLifecycleRepository struct{ ports.InvoiceRepository }
type invoiceLifecycleTransactions struct{ ports.TransactionManager }

func TestBuildInvoicesStartsWorkerOnlyInEnabledWorkerModes(t *testing.T) {
	for _, tc := range []struct {
		name         string
		mode         string
		workerEnable bool
		invoices     bool
		wantService  bool
		wantWorker   bool
	}{
		{name: "api only", mode: "api", workerEnable: true, invoices: true, wantService: true},
		{name: "worker", mode: "worker", workerEnable: true, invoices: true, wantService: true, wantWorker: true},
		{name: "all", mode: "all", workerEnable: true, invoices: true, wantService: true, wantWorker: true},
		{name: "worker disabled", mode: "worker", invoices: true, wantService: true},
		{name: "invoice capability disabled", mode: "all"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, merchants := invoiceConfig(t)
			cfg.Invoices.Enabled = tc.invoices
			cfg.Invoices.WorkerEnabled = tc.workerEnable
			var merchantRepo ports.MerchantRepository
			var invoiceRepo ports.InvoiceRepository
			var tx ports.TransactionManager
			if tc.invoices {
				merchantRepo = merchants
				invoiceRepo = invoiceLifecycleRepository{}
				tx = invoiceLifecycleTransactions{}
			}
			components, err := buildInvoices(t.Context(), cfg, tc.mode, merchantRepo, invoiceRepo, tx, time.Now, nil)
			if err != nil {
				t.Fatal(err)
			}
			if (components.service != nil) != tc.wantService || (components.worker != nil) != tc.wantWorker {
				t.Fatalf("invoice components service=%v worker=%v", components.service != nil, components.worker != nil)
			}
		})
	}
}

type outboxStopRecorder struct {
	order *[]string
	err   error
}

func (r outboxStopRecorder) Start(context.Context) {}

func (r outboxStopRecorder) Stop(context.Context) error {
	*r.order = append(*r.order, "outbox")
	return r.err
}

func TestShutdownStopsInvoiceWorkerBeforeClosingPool(t *testing.T) {
	var order []string
	err := shutdownRuntime(t.Context(), func(context.Context) error {
		order = append(order, "invoice")
		return nil
	}, outboxStopRecorder{order: &order}, nil, []func(){func() { order = append(order, "pool") }})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"invoice", "outbox", "pool"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("shutdown order=%v want=%v", order, want)
	}
}

func TestShutdownKeepsPoolOpenWhenInvoiceWorkerDoesNotStop(t *testing.T) {
	var order []string
	stopErr := context.DeadlineExceeded
	err := shutdownRuntime(t.Context(), func(context.Context) error {
		order = append(order, "invoice")
		return stopErr
	}, outboxStopRecorder{order: &order}, nil, []func(){func() { order = append(order, "pool") }})
	if !errors.Is(err, stopErr) {
		t.Fatalf("shutdown error=%v", err)
	}
	if want := []string{"invoice", "outbox"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("shutdown order after timeout=%v want=%v", order, want)
	}
}
