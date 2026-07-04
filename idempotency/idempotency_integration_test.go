//go:build integration

// Integration tests for the idempotency Repository against a real Postgres via
// testcontainers-go. Run with:
//
//	go test -tags=integration ./idempotency/...
//
// Requires a reachable Docker daemon.
package idempotency

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

const schema = `
CREATE TABLE idempotency_keys (
	id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	user_id        BIGINT      NOT NULL,
	idem_key       TEXT        NOT NULL,
	request_method TEXT        NOT NULL,
	request_path   TEXT        NOT NULL,
	request_hash   TEXT        NOT NULL,
	locked_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
	subject_id     BIGINT,
	response_code  INT,
	response_body  JSONB,
	created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
	UNIQUE (user_id, idem_key)
);`

func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("idem"), postgres.WithUsername("idem"), postgres.WithPassword("secret"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(90*time.Second)),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(ctx) })
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return pool
}

func TestRepository_Integration(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	repo := New(pool, 90*time.Second)

	t.Run("fresh claim then replay after finish", func(t *testing.T) {
		rec, fresh, err := repo.Claim(ctx, 1, "k1", "POST", "/pay", "h1")
		if err != nil || !fresh {
			t.Fatalf("fresh claim: fresh=%v err=%v", fresh, err)
		}
		if err := repo.Finish(ctx, rec.ID, 201, []byte(`{"ok":true}`)); err != nil {
			t.Fatal(err)
		}
		replay, fresh, err := repo.Claim(ctx, 1, "k1", "POST", "/pay", "h1")
		if err != nil || fresh {
			t.Fatalf("replay must be non-fresh: fresh=%v err=%v", fresh, err)
		}
		// jsonb canonicalizes the body (whitespace differs), so compare semantically.
		var got map[string]any
		if err := json.Unmarshal(replay.ResponseBody, &got); err != nil {
			t.Fatalf("cached body not valid JSON: %v", err)
		}
		if !replay.Finished() || *replay.ResponseCode != 201 || got["ok"] != true {
			t.Fatalf("replay must carry the cached response: code=%v body=%v", replay.ResponseCode, got)
		}
	})

	t.Run("same key different request is a conflict", func(t *testing.T) {
		if _, _, err := repo.Claim(ctx, 2, "k2", "POST", "/pay", "h1"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := repo.Claim(ctx, 2, "k2", "POST", "/pay", "DIFFERENT"); err != ErrConflict {
			t.Fatalf("hash mismatch → ErrConflict, got %v", err)
		}
		if _, _, err := repo.Claim(ctx, 2, "k2", "POST", "/other", "h1"); err != ErrConflict {
			t.Fatalf("path mismatch → ErrConflict, got %v", err)
		}
	})

	t.Run("in-flight fresh lock is locked", func(t *testing.T) {
		if _, _, err := repo.Claim(ctx, 3, "k3", "POST", "/pay", "h1"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := repo.Claim(ctx, 3, "k3", "POST", "/pay", "h1"); err != ErrLocked {
			t.Fatalf("in-flight fresh lock → ErrLocked, got %v", err)
		}
	})

	t.Run("checkpoint sets subject; release enables takeover", func(t *testing.T) {
		rec, _, err := repo.Claim(ctx, 4, "k4", "POST", "/pay", "h1")
		if err != nil {
			t.Fatal(err)
		}
		sid := int64(777)
		if err := repo.Checkpoint(ctx, rec.ID, &sid); err != nil {
			t.Fatal(err)
		}
		if err := repo.Release(ctx, rec.ID); err != nil {
			t.Fatal(err)
		}
		// After release the lock is aged → a same-key retry takes it over.
		took, fresh, err := repo.Claim(ctx, 4, "k4", "POST", "/pay", "h1")
		if err != nil || !fresh {
			t.Fatalf("released lock must be taken over: fresh=%v err=%v", fresh, err)
		}
		if took.SubjectID == nil || *took.SubjectID != 777 {
			t.Fatalf("takeover must adopt the checkpointed subject, got %v", took.SubjectID)
		}
	})

	t.Run("stale lock is taken over", func(t *testing.T) {
		rec, _, err := repo.Claim(ctx, 5, "k5", "POST", "/pay", "h1")
		if err != nil {
			t.Fatal(err)
		}
		// Age the lock past the takeover window directly.
		if _, err := pool.Exec(ctx, `UPDATE idempotency_keys SET locked_at = now() - interval '10 minutes' WHERE id=$1`, rec.ID); err != nil {
			t.Fatal(err)
		}
		if _, fresh, err := repo.Claim(ctx, 5, "k5", "POST", "/pay", "h1"); err != nil || !fresh {
			t.Fatalf("stale lock → takeover (fresh), got fresh=%v err=%v", fresh, err)
		}
	})

	t.Run("reap deletes keys past ttl", func(t *testing.T) {
		rec, _, err := repo.Claim(ctx, 6, "k6", "POST", "/pay", "h1")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE idempotency_keys SET created_at = now() - interval '48 hours' WHERE id=$1`, rec.ID); err != nil {
			t.Fatal(err)
		}
		n, err := repo.Reap(ctx, 24*time.Hour)
		if err != nil || n < 1 {
			t.Fatalf("reap should remove the aged key: n=%d err=%v", n, err)
		}
	})
}
