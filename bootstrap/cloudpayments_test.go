package bootstrap

import (
	cp "github.com/Seraf-seraf/payment/adapter/provider/cloudpayments"
	"github.com/Seraf-seraf/payment/pkg/config"
	"github.com/Seraf-seraf/payment/ports"
	"testing"
	"time"
)

type cloudPaymentsReader struct{ ports.PaymentRepository }

func TestCloudPaymentsRegistryWiring(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatal(err)
	}
	registry, err := buildProviderRegistry(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.Get("cloudpayments"); ok {
		t.Fatal("disabled provider registered")
	}
	if _, ok := registry.Get("mock"); !ok {
		t.Fatal("existing provider missing")
	}
	cfg.Providers.CloudPayments = config.CloudPayments{Enabled: true, APIURL: "http://127.0.0.1:9999", PublicID: "public", APISecret: "secret", Timeout: time.Second, TestMode: true}
	registry, err = buildProviderRegistry(cfg, cloudPaymentsReader{})
	if err != nil {
		t.Fatal(err)
	}
	p, ok := registry.Get("cloudpayments")
	if !ok {
		t.Fatal("enabled provider missing")
	}
	if _, ok := p.(*cp.Provider); !ok {
		t.Fatal("wrong provider implementation")
	}
	if _, err := buildProviderRegistry(cfg, nil); err == nil {
		t.Fatal("missing payment reader accepted")
	}
	cfg.Providers.CloudPayments.APISecret = ""
	if _, err := buildProviderRegistry(cfg, cloudPaymentsReader{}); err == nil {
		t.Fatal("missing credential accepted")
	}
}
