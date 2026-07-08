package config

import (
	"errors"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

const DefaultPath = "configs/config.yaml"

type Config struct {
	App       App       `yaml:"app"`
	HTTP      HTTP      `yaml:"http"`
	Postgres  Postgres  `yaml:"postgres"`
	Security  Security  `yaml:"security"`
	Providers Providers `yaml:"providers"`
	Outbox    Outbox    `yaml:"outbox"`
}

type App struct {
	Name string `yaml:"name"`
	Env  string `yaml:"env"`
	Mode string `yaml:"mode"`
}

type HTTP struct {
	Addr              string        `yaml:"addr"`
	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	ShutdownTimeout   time.Duration `yaml:"shutdown_timeout"`
}

type Postgres struct {
	DSN             string        `yaml:"dsn"`
	MaxOpenConns    int32         `yaml:"max_open_conns"`
	MaxIdleConns    int32         `yaml:"max_idle_conns"`
	ConnMaxLifetime time.Duration `yaml:"conn_max_lifetime"`
}

type Security struct {
	HMACMaxSkew time.Duration `yaml:"hmac_max_skew"`
}

type Providers struct {
	Default string       `yaml:"default"`
	Mock    MockProvider `yaml:"mock"`
	TBank   TBank        `yaml:"tbank"`
}

type MockProvider struct {
	Enabled       bool   `yaml:"enabled"`
	WebhookSecret string `yaml:"webhook_secret"`
}

type TBank struct {
	Enabled       bool   `yaml:"enabled"`
	APIURL        string `yaml:"api_url"`
	TerminalKey   string `yaml:"terminal_key"`
	Password      string `yaml:"password"`
	WebhookSecret string `yaml:"webhook_secret"`
}

type Outbox struct {
	Enabled      bool          `yaml:"enabled"`
	PollInterval time.Duration `yaml:"poll_interval"`
	BatchSize    int           `yaml:"batch_size"`
	MaxAttempts  int           `yaml:"max_attempts"`
	WorkerCount  int           `yaml:"worker_count"`
}

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
	return cfg, nil
}

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
		Outbox: Outbox{
			Enabled:      true,
			PollInterval: time.Second,
			BatchSize:    100,
			MaxAttempts:  10,
			WorkerCount:  1,
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
	if value := os.Getenv("TBANK_WEBHOOK_SECRET"); value != "" {
		cfg.Providers.TBank.WebhookSecret = value
	}
}
