//go:build integration

package postgres

import (
	"cmp"
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
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
	if err != nil {
		fmt.Fprintf(os.Stderr, "start %s: %v\n", image, err)
		return 1
	}

	defer func() {
		if err := testcontainers.TerminateContainer(ctr); err != nil {
			fmt.Fprintf(os.Stderr, "stop %s: %v\n", image, err)
		}
	}()

	adminURL, err = ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "connection string: %v\n", err)
		return 1
	}

	return m.Run()
}

// newDatabase creates an empty database and returns its URL. The container's end removes it.
func newDatabase(tb testing.TB) string {
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

// openStore opens a Store closed at the end of the test.
func openStore(tb testing.TB, connString string) *Store {
	tb.Helper()

	s, err := Open(tb.Context(), connString)
	if err != nil {
		tb.Fatalf("Open: %v", err)
	}

	tb.Cleanup(s.Close)

	return s
}

// together starts every fn at once and returns their errors in fns order.
func together(fns ...func() error) []error {
	errs := make([]error, len(fns))
	start := make(chan struct{})

	var wg sync.WaitGroup

	for i, fn := range fns {
		wg.Go(func() {
			<-start

			errs[i] = fn()
		})
	}

	close(start)
	wg.Wait()

	return errs
}
