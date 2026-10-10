// Package testutil provides helpers for integration tests that need a real
// PostgreSQL. Set ASTRION_TEST_DATABASE_URL to an admin DSN (e.g.
// postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable). Each test
// gets its own freshly created database, dropped on cleanup.
package testutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

// NewDatabase creates an isolated database and returns its DSN, or skips.
func NewDatabase(t testing.TB) string {
	t.Helper()
	admin := os.Getenv("ASTRION_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("ASTRION_TEST_DATABASE_URL not set; skipping PostgreSQL integration test")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("connect admin db: %v", err)
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "astrion_test_" + hex.EncodeToString(b)
	if _, err := conn.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		conn.Close(ctx)
		t.Fatalf("create database: %v", err)
	}
	conn.Close(ctx)
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), admin)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
	})
	return u.String()
}
