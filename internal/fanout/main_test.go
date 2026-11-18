//go:build integration

package fanout

import (
	"cmp"
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/fervovita/AuthZ-dev/internal/datastore/postgres"
)

// adminURL reaches the container's own database, where each test creates one of its own.
var adminURL string

var databases atomic.Int64

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

// run holds the container for the whole package, so it is stopped before TestMain exits.
func run(m *testing.M) int {
	ctx := context.Background()

	image := cmp.Or(os.Getenv("POSTGRES_TEST_IMAGE"), "postgres:18-alpine")

	ctr, err := tcpostgres.Run(ctx, image, tcpostgres.BasicWaitStrategies())

	// Run can return a container it failed to start, so it is stopped whatever err is.
	defer func() {
		if err := testcontainers.TerminateContainer(ctr); err != nil {
			fmt.Fprintf(os.Stderr, "stop %s: %v\n", image, err)
		}
	}()

	if err != nil {
		fmt.Fprintf(os.Stderr, "start %s: %v\n", image, err)
		return 1
	}

	adminURL, err = ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "connection string: %v\n", err)
		return 1
	}

	return m.Run()
}

// database creates a database of the test's own and returns its URL.
func database(tb testing.TB) string {
	tb.Helper()

	admin, err := pgx.Connect(tb.Context(), adminURL)
	if err != nil {
		tb.Fatalf("connect: %v", err)
	}

	defer func() { _ = admin.Close(context.Background()) }()

	name := "test_" + strconv.FormatInt(databases.Add(1), 10)
	if _, err := admin.Exec(tb.Context(), "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		tb.Fatalf("create database: %v", err)
	}

	u, err := url.Parse(adminURL)
	if err != nil {
		tb.Fatalf("parse %q: %v", adminURL, err)
	}

	u.Path = "/" + name

	return u.String()
}

// centralStore opens a migrated store on the database at dsn, closed at the end of the test, with source written.
func centralStore(tb testing.TB, dsn, source string) *postgres.Store {
	tb.Helper()

	st, err := postgres.Open(tb.Context(), dsn)
	if err != nil {
		tb.Fatalf("Open: %v", err)
	}

	tb.Cleanup(st.Close)

	if err := st.Migrate(tb.Context()); err != nil {
		tb.Fatalf("Migrate: %v", err)
	}

	if err := st.WriteSchema(tb.Context(), source); err != nil {
		tb.Fatalf("WriteSchema: %v", err)
	}

	return st
}

// runHub runs h until the test ends.
func runHub(tb testing.TB, h *Hub) {
	tb.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)

		_ = h.Run(ctx)
	}()

	tb.Cleanup(func() {
		cancel()
		<-done
	})
}
