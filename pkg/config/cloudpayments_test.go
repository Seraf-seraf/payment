package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCloudPaymentsConfigurationDefaults(t *testing.T) {
	t.Setenv("CLOUDPAYMENTS_PUBLIC_ID", "")
	t.Setenv("CLOUDPAYMENTS_API_SECRET", "")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	p := c.Providers.CloudPayments
	if p.Enabled || p.APIURL != "https://api.cloudpayments.ru" || p.PublicID != "" || p.APISecret != "" || !p.TestMode || p.Timeout != 10*time.Second {
		t.Fatalf("cloudpayments defaults: enabled=%v URL=%s test=%v timeout=%s", p.Enabled, p.APIURL, p.TestMode, p.Timeout)
	}
	if !c.Providers.Mock.Enabled || c.Providers.Default != "mock" || c.Providers.TBank.Enabled || c.Providers.TBank.APIURL != "https://securepay.tinkoff.ru/v2" {
		t.Fatal("legacy defaults changed")
	}
}
func TestCloudPaymentsConfigurationYAMLAndCredentialsOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	err := os.WriteFile(path, []byte("providers:\n  cloudpayments:\n    enabled: true\n    api_url: http://localhost:9999\n    public_id: yaml-public\n    api_secret: yaml-secret\n    test_mode: false\n    timeout: 3s\n"), 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLOUDPAYMENTS_PUBLIC_ID", "")
	t.Setenv("CLOUDPAYMENTS_API_SECRET", "")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p := c.Providers.CloudPayments
	if !p.Enabled || p.PublicID != "yaml-public" || p.APISecret != "yaml-secret" || p.APIURL != "http://localhost:9999" || p.TestMode || p.Timeout != 3*time.Second {
		t.Fatal("YAML values lost")
	}
	t.Setenv("CLOUDPAYMENTS_PUBLIC_ID", "env-public")
	t.Setenv("CLOUDPAYMENTS_API_SECRET", "env-secret")
	c, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Providers.CloudPayments.PublicID != "env-public" || c.Providers.CloudPayments.APISecret != "env-secret" {
		t.Fatal("credential env override lost")
	}
}
