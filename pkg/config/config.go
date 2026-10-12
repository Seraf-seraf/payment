package config

import (
	"errors"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultPath задает путь к конфигу по умолчанию.
const DefaultPath = "configs/config.yaml"

// Config содержит все настройки приложения.
type Config struct {
	App       App       `yaml:"app"`
	HTTP      HTTP      `yaml:"http"`
	Postgres  Postgres  `yaml:"postgres"`
	Security  Security  `yaml:"security"`
	Providers Providers `yaml:"providers"`
	Invoices  Invoices  `yaml:"invoices"`
	Outbox    Outbox    `yaml:"outbox"`
	Callback  Callback  `yaml:"callback"`
}

// App содержит общие настройки приложения.
type App struct {
	Name string `yaml:"name"`
	Env  string `yaml:"env"`
	Mode string `yaml:"mode"`
}

// HTTP содержит настройки HTTP-сервера.
type HTTP struct {
	Addr              string        `yaml:"addr"`
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	ShutdownTimeout   time.Duration `yaml:"shutdown_timeout"`
}

// Postgres содержит настройки подключения к PostgreSQL.
type Postgres struct {
	DSN             string        `yaml:"dsn"`
	MaxOpenConns    int32         `yaml:"max_open_conns"`
	MaxIdleConns    int32         `yaml:"max_idle_conns"`
	ConnMaxLifetime time.Duration `yaml:"conn_max_lifetime"`
}

// Security содержит настройки безопасности запросов и секретов.
type Security struct {
	HMACMaxSkew   time.Duration `yaml:"hmac_max_skew"`
	EncryptionKey string        `yaml:"encryption_key"`
}

// Providers содержит настройки платежных провайдеров.
type Providers struct {
	Default string       `yaml:"default"`
	Mock    MockProvider `yaml:"mock"`
	TBank   TBank        `yaml:"tbank"`
}

type Invoices struct {
	Enabled                  bool                `yaml:"enabled"`
	WorkerEnabled            bool                `yaml:"worker_enabled"`
	PollInterval             time.Duration       `yaml:"poll_interval"`
	CommandBatchSize         int                 `yaml:"command_batch_size"`
	CommandLease             time.Duration       `yaml:"command_lease"`
	ProviderTimeout          time.Duration       `yaml:"provider_timeout"`
	MaxAttempts              int                 `yaml:"max_attempts"`
	ReconcileInterval        time.Duration       `yaml:"reconcile_interval"`
	ExpiredReconcileInterval time.Duration       `yaml:"expired_reconcile_interval"`
	Connections              []InvoiceConnection `yaml:"connections"`
}

type InvoiceConnection struct {
	ID                  string             `yaml:"id"`
	MerchantID          string             `yaml:"merchant_id"`
	ProviderKind        string             `yaml:"provider_kind"`
	EvidenceMode        string             `yaml:"evidence_mode"`
	CreationReplay      string             `yaml:"creation_replay"`
	WebhookSecretEnv    string             `yaml:"webhook_secret_env"`
	Beneficiary         InvoiceBeneficiary `yaml:"beneficiary"`
	OperationIDStrategy string             `yaml:"operation_id_strategy"`
	OperationAliases    map[string]string  `yaml:"operation_aliases"`
}

type InvoiceBeneficiary struct {
	Name                 string `yaml:"name"`
	INN                  string `yaml:"inn"`
	KPP                  string `yaml:"kpp"`
	Account              string `yaml:"account"`
	BankBIK              string `yaml:"bank_bik"`
	BankName             string `yaml:"bank_name"`
	CorrespondentAccount string `yaml:"correspondent_account"`
}

func (c Invoices) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.PollInterval <= 0 || c.CommandBatchSize < 1 || c.CommandBatchSize > 100 || c.ProviderTimeout <= 0 || c.CommandLease <= c.ProviderTimeout || c.MaxAttempts < 1 || c.MaxAttempts > 100 || c.ReconcileInterval <= 0 || c.ExpiredReconcileInterval <= 0 {
		return errors.New("invalid invoices worker configuration")
	}
	return nil
}

// MockProvider содержит настройки тестового провайдера.
type MockProvider struct {
	Enabled       bool   `yaml:"enabled"`
	WebhookSecret string `yaml:"webhook_secret"`
}

// TBank содержит настройки T-Bank провайдера.
type TBank struct {
	Enabled         bool   `yaml:"enabled"`
	APIURL          string `yaml:"api_url"`
	TerminalKey     string `yaml:"terminal_key"`
	Password        string `yaml:"password"`
	NotificationURL string `yaml:"notification_url"`
}

// Outbox содержит настройки worker доставки callback-событий.
type Outbox struct {
	Enabled      bool          `yaml:"enabled"`
	PollInterval time.Duration `yaml:"poll_interval"`
	BatchSize    int           `yaml:"batch_size"`
	MaxAttempts  int           `yaml:"max_attempts"`
	WorkerCount  int           `yaml:"worker_count"`
}

// Callback содержит настройки исходящих callback-запросов.
type Callback struct {
	Timeout time.Duration `yaml:"timeout"`
}

// Load загружает конфигурацию из YAML-файла и применяет env override.
func Load(path string) (Config, error) {
	cfg := defaultConfig()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return Config{}, err
		}
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return Config{}, err
		}
	}
	applyEnv(&cfg)
	if cfg.HTTP.Addr == "" {
		return Config{}, errors.New("http addr is required")
	}
	if cfg.App.Name == "" {
		return Config{}, errors.New("app name is required")
	}
	if err := cfg.Invoices.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// LoadFromEnv загружает конфигурацию по пути из CONFIG_PATH или из DefaultPath.
func LoadFromEnv() (Config, error) {
	path := os.Getenv("CONFIG_PATH")
	if path == "" {
		path = DefaultPath
	}
	return Load(path)
}

func defaultConfig() Config {
	return Config{
		App: App{
			Name: "payment-service",
			Env:  "local",
			Mode: "all",
		},
		HTTP: HTTP{
			Addr:              ":8080",
			ReadHeaderTimeout: 5 * time.Second,
			ShutdownTimeout:   10 * time.Second,
		},
		Postgres: Postgres{
			DSN:             "postgres://payment:payment@localhost:5432/payment?sslmode=disable",
			MaxOpenConns:    10,
			MaxIdleConns:    5,
			ConnMaxLifetime: 30 * time.Minute,
		},
		Security: Security{
			HMACMaxSkew: 5 * time.Minute,
		},
		Providers: Providers{
			Default: "mock",
			Mock: MockProvider{
				Enabled:       true,
				WebhookSecret: "mock-webhook-secret",
			},
			TBank: TBank{
				Enabled: false,
				APIURL:  "https://securepay.tinkoff.ru/v2",
			},
		},
		Invoices: Invoices{Enabled: false, WorkerEnabled: true, PollInterval: 60 * time.Second, CommandBatchSize: 20, CommandLease: 60 * time.Second, ProviderTimeout: 10 * time.Second, MaxAttempts: 10, ReconcileInterval: 10 * time.Minute, ExpiredReconcileInterval: 24 * time.Hour},
		Outbox: Outbox{
			Enabled:      true,
			PollInterval: time.Second,
			BatchSize:    100,
			MaxAttempts:  10,
			WorkerCount:  1,
		},
		Callback: Callback{
			Timeout: 5 * time.Second,
		},
	}
}

func applyEnv(cfg *Config) {
	if value := os.Getenv("APP_MODE"); value != "" {
		cfg.App.Mode = value
	}
	if value := os.Getenv("HTTP_ADDR"); value != "" {
		cfg.HTTP.Addr = value
	}
	if value := os.Getenv("TBANK_TERMINAL_KEY"); value != "" {
		cfg.Providers.TBank.TerminalKey = value
	}
	if value := os.Getenv("TBANK_PASSWORD"); value != "" {
		cfg.Providers.TBank.Password = value
	}
	if value := os.Getenv("TBANK_NOTIFICATION_URL"); value != "" {
		cfg.Providers.TBank.NotificationURL = value
	}
	if value := os.Getenv("SECURITY_ENCRYPTION_KEY"); value != "" {
		cfg.Security.EncryptionKey = value
	}
}
