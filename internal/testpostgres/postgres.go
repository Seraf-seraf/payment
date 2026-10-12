//go:build functional

package testpostgres

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

const databaseName = "payment_test"

var containerDSN string

func Run(m *testing.M) (code int) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, err := postgres.Run(ctx, "postgres:16.13-alpine3.23",
		postgres.WithDatabase(databaseName), postgres.WithUsername("payment_test"),
		postgres.WithPassword(uuid.NewString()), postgres.BasicWaitStrategies())
	if container != nil {
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := container.Terminate(ctx); err != nil {
				fmt.Fprintln(os.Stderr, "terminate PostgreSQL testcontainer:", err)
				code = 1
			}
		}()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "start PostgreSQL testcontainer:", err)
		return 1
	}
	defer func() { containerDSN = "" }()
	containerDSN, err = container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintln(os.Stderr, "get PostgreSQL testcontainer connection:", err)
		return 1
	}
	return m.Run()
}

func Schema(t *testing.T, migrationsPath string) (*pgxpool.Pool, *goose.Provider) {
	t.Helper()
	if containerDSN == "" {
		t.Fatal("PostgreSQL testcontainer must be initialized by TestMain")
	}
	config, err := pgxpool.ParseConfig(containerDSN)
	must(t, err)
	if config.ConnConfig.Database != databaseName {
		t.Fatal("unexpected testcontainer database")
	}
	ctx := t.Context()
	root, err := pgxpool.NewWithConfig(ctx, config.Copy())
	must(t, err)
	t.Cleanup(root.Close)
	var actualDatabase string
	must(t, root.QueryRow(ctx, "SELECT current_database()").Scan(&actualDatabase))
	if actualDatabase != databaseName {
		t.Fatal("unexpected connected testcontainer database")
	}
	schema := "invoice_test_" + uuid.New().String()
	if schema == "public" {
		t.Fatal("public schema cannot be used for tests")
	}
	quoted := pgx.Identifier{schema}.Sanitize()
	_, err = root.Exec(ctx, "CREATE SCHEMA "+quoted)
	must(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := root.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error("drop test schema:", err)
		}
	})
	config.ConnConfig.RuntimeParams["search_path"] = quoted
	config.MaxConns = 24
	pool, err := pgxpool.NewWithConfig(ctx, config)
	must(t, err)
	t.Cleanup(pool.Close)
	var actualSchema string
	must(t, pool.QueryRow(ctx, "SELECT current_schema()").Scan(&actualSchema))
	if actualSchema != schema {
		t.Fatal("unexpected test schema")
	}
	db := stdlib.OpenDB(*config.ConnConfig)
	t.Cleanup(func() { must(t, db.Close()) })
	migrations, err := goose.NewProvider(goose.DialectPostgres, db, os.DirFS(migrationsPath))
	must(t, err)
	_, err = migrations.Up(ctx)
	must(t, err)
	return pool, migrations
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
