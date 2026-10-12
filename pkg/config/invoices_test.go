package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInvoicesDefaultsAndExample(t *testing.T) {
	for _, path := range []string{"", "../../configs/config.example.yaml"} {
		cfg, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		c := cfg.Invoices
		if c.Enabled || !c.WorkerEnabled || c.PollInterval != 60*time.Second || c.CommandBatchSize != 20 || c.CommandLease != 60*time.Second || c.ProviderTimeout != 10*time.Second || c.MaxAttempts != 10 || c.ReconcileInterval != 10*time.Minute || c.ExpiredReconcileInterval != 24*time.Hour {
			t.Fatalf("defaults %s: %+v", path, c)
		}
	}
}
func TestInvoicesLeaseTimeoutValidation(t *testing.T) {
	for _, lease := range []string{"9s", "10s", "11s"} {
		t.Run(lease, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte("invoices:\n  enabled: true\n  command_lease: "+lease+"\n  provider_timeout: 10s\n"), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if (err == nil) != (lease == "11s") {
				t.Fatalf("lease %s: %v", lease, err)
			}
		})
	}
}
