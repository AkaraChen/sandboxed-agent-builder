// Package pgtest gives tests a fresh PostgreSQL database each.
//
// The server is SAB_TEST_DATABASE_URL when set. Otherwise a local
// container, sab-test-postgres (postgres:17-alpine on 127.0.0.1:55432), is
// started on first use and left running for later test runs; remove it
// with `docker rm -f sab-test-postgres`.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	container = "sab-test-postgres"
	localURL  = "postgres://postgres:postgres@127.0.0.1:55432/postgres?sslmode=disable"
)

var (
	once      sync.Once
	serverURL string
	serverErr error
)

func server() (string, error) {
	once.Do(func() {
		serverURL = os.Getenv("SAB_TEST_DATABASE_URL")
		if serverURL != "" {
			return
		}
		serverURL = localURL
		if ping(serverURL) == nil {
			return
		}
		// Another test binary may be starting it too; a name conflict is
		// fine, the wait below covers both.
		_ = exec.Command("docker", "run", "-d", "--name", container, "-p", "127.0.0.1:55432:5432",
			"-e", "POSTGRES_PASSWORD=postgres", "postgres:17-alpine").Run()
		deadline := time.Now().Add(60 * time.Second)
		for {
			if serverErr = ping(serverURL); serverErr == nil || time.Now().After(deadline) {
				return
			}
			time.Sleep(300 * time.Millisecond)
		}
	})
	return serverURL, serverErr
}

func ping(u string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, u)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	return conn.Ping(ctx)
}

// URL creates an empty database for t, dropped when t ends, and returns
// its connection URL.
func URL(t testing.TB) string {
	t.Helper()
	base, err := server()
	if err != nil {
		t.Fatalf("no PostgreSQL for tests (set SAB_TEST_DATABASE_URL or make docker available): %v", err)
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "sab_test_" + hex.EncodeToString(b)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		conn, err := pgx.Connect(ctx, base)
		if err == nil {
			_, _ = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			conn.Close(ctx)
		}
	})
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}
