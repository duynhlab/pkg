package idempotency

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository persists idempotency records. The UNIQUE(user_id, idem_key) index
// is the race-free claim: INSERT ... ON CONFLICT DO NOTHING, and rows-affected
// decides the winner.
type Repository struct {
	pool         *pgxpool.Pool
	lockTakeover time.Duration
}

// New wires the repository; lockTakeover is how stale an in-flight lock must be
// before a new attempt may take it over.
func New(pool *pgxpool.Pool, lockTakeover time.Duration) *Repository {
	return &Repository{pool: pool, lockTakeover: lockTakeover}
}

const columns = `id, user_id, idem_key, request_method, request_path, request_hash,
	locked_at, subject_id, response_code, response_body, created_at`

func scan(row pgx.Row) (*Record, error) {
	var r Record
	err := row.Scan(&r.ID, &r.UserID, &r.Key, &r.RequestMethod, &r.RequestPath,
		&r.RequestHash, &r.LockedAt, &r.SubjectID,
		&r.ResponseCode, &r.ResponseBody, &r.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan idempotency record: %w", err)
	}
	return &r, nil
}

// Claim atomically claims (userID, key) for this request. Outcomes:
//   - fresh claim                     -> (rec, true, nil): caller proceeds
//   - finished + same request         -> (rec, false, nil): caller replays cached response
//   - finished/in-flight, other request -> ErrConflict
//   - in-flight, fresh lock           -> ErrLocked
//   - in-flight, stale lock           -> (rec, true, nil): TAKEOVER — caller re-drives,
//     reusing the checkpointed subject id
func (r *Repository) Claim(ctx context.Context, userID int64, key, method, path, hash string) (*Record, bool, error) {
	tag, err := r.pool.Exec(ctx, `
		INSERT INTO idempotency_keys (user_id, idem_key, request_method, request_path, request_hash)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (user_id, idem_key) DO NOTHING`,
		userID, key, method, path, hash)
	if err != nil {
		return nil, false, fmt.Errorf("claim idempotency key: %w", err)
	}

	existing, err := scan(r.pool.QueryRow(ctx,
		`SELECT `+columns+` FROM idempotency_keys WHERE user_id = $1 AND idem_key = $2`,
		userID, key))
	if err != nil {
		return nil, false, err
	}

	if tag.RowsAffected() == 1 {
		return existing, true, nil // fresh claim — we won the index race
	}

	// A key identifies ONE request: same key on a different endpoint or with a
	// different body is a conflict, never a replay. Path+method scoping keeps a
	// key from ever answering a different endpoint whose body shape collides.
	if existing.RequestHash != hash || existing.RequestPath != path || existing.RequestMethod != method {
		return nil, false, ErrConflict
	}
	if existing.Finished() {
		return existing, false, nil // replay
	}

	// In-flight: fresh lock waits; stale lock is taken over.
	if time.Since(existing.LockedAt) < r.lockTakeover {
		return nil, false, ErrLocked
	}
	took, err := scan(r.pool.QueryRow(ctx, `
		UPDATE idempotency_keys SET locked_at = now()
		WHERE id = $1 AND locked_at = $2 AND response_code IS NULL
		RETURNING `+columns,
		existing.ID, existing.LockedAt))
	if errors.Is(err, ErrNotFound) {
		return nil, false, ErrLocked // someone else took it over first
	}
	if err != nil {
		return nil, false, err
	}
	return took, true, nil
}

// Checkpoint records the subject this key created and refreshes the lock, so a
// crash-recovery takeover adopts the existing subject instead of creating a
// second one.
func (r *Repository) Checkpoint(ctx context.Context, id int64, subjectID *int64) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE idempotency_keys SET subject_id = COALESCE($2, subject_id), locked_at = now()
		WHERE id = $1`, id, subjectID)
	if err != nil {
		return fmt.Errorf("checkpoint idempotency key: %w", err)
	}
	return nil
}

// Release ages the lock so an immediate same-key retry is treated as a takeover
// instead of ErrLocked — used when an attempt fails transiently and the caller
// is told to retry. Only affects still-in-flight keys.
func (r *Repository) Release(ctx context.Context, id int64) error {
	_, err := r.pool.Exec(ctx,
		`UPDATE idempotency_keys SET locked_at = 'epoch' WHERE id = $1 AND response_code IS NULL`, id)
	if err != nil {
		return fmt.Errorf("release idempotency key: %w", err)
	}
	return nil
}

// Finish stores the cached response and marks the key finished — the replay
// source for every later duplicate. The body binds as text with an explicit
// ::jsonb cast: under the simple query protocol (PgBouncer/PgDog pools) a raw
// []byte parameter is sent as a bytea hex literal, which jsonb rejects.
func (r *Repository) Finish(ctx context.Context, id int64, code int, body []byte) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE idempotency_keys SET response_code = $2, response_body = $3::jsonb
		WHERE id = $1`, id, code, string(body))
	if err != nil {
		return fmt.Errorf("finish idempotency key: %w", err)
	}
	return nil
}

// Reap deletes keys older than ttl (Stripe's 24h window). Returns rows removed.
func (r *Repository) Reap(ctx context.Context, ttl time.Duration) (int64, error) {
	tag, err := r.pool.Exec(ctx,
		`DELETE FROM idempotency_keys WHERE created_at < now() - $1::interval`,
		fmt.Sprintf("%d seconds", int(ttl.Seconds())))
	if err != nil {
		return 0, fmt.Errorf("reap idempotency keys: %w", err)
	}
	return tag.RowsAffected(), nil
}
