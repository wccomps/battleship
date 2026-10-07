// Package storetest gives tests a Store on a fresh Postgres database.
package storetest

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wccomps/battleship/internal/store"
)

// EnvVar names the Postgres URL tests use. It must point at a server where the
// user may create and drop databases, e.g.
// postgres://postgres:test@127.0.0.1:55432/postgres?sslmode=disable
const EnvVar = "BATTLESHIP_TEST_DATABASE_URL"

// New returns a Store on a new, migrated database that is dropped when the
// test ends. It skips the test when EnvVar is unset.
func New(t testing.TB) *store.Store {
	t.Helper()
	s, _ := NewWithURL(t)
	return s
}

// NewWithURL is New plus the database URL, for tests needing their own
// connection.
func NewWithURL(t testing.TB) (*store.Store, string) {
	t.Helper()
	admin := os.Getenv(EnvVar)
	switch {
	case admin == "" && os.Getenv("CI") != "":
		// CI must run every database test; a skip there is lost coverage.
		t.Fatalf("%s not set in CI", EnvVar)
	case admin == "":
		t.Skipf("%s not set; see README (Tests) to run database tests", EnvVar)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("connecting to %s: %v", EnvVar, err)
	}
	defer conn.Close(ctx)
	name := fmt.Sprintf("battleship_test_%d_%d", time.Now().UnixNano(), rand.IntN(1_000_000))
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("creating test database: %v", err)
	}

	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	s, err := store.Open(ctx, u.String(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c, err := pgx.Connect(ctx, admin)
		if err != nil {
			t.Errorf("dropping test database: %v", err)
			return
		}
		defer c.Close(ctx)
		if _, err := c.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("dropping test database: %v", err)
		}
	})
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrating: %v", err)
	}
	return s, u.String()
}
