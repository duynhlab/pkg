// Package idempotency provides a Postgres-backed idempotency store with
// Stripe-style semantics: a client key claims one request; the first response is
// cached and replayed verbatim to duplicates; the same key with a different
// request is a conflict; concurrent duplicates are serialized by an in-flight
// lock with stale-lock takeover for crash recovery.
//
// It is transport- and domain-agnostic: a request is identified by
// (user_id, key, method, path, hash), and SubjectID is an opaque caller-owned
// reference (e.g. the row a POST created) checkpointed so a takeover re-drives
// against the same subject instead of creating a second one.
//
// The caller owns the table (this package ships no migrations). Required schema:
//
//	CREATE TABLE idempotency_keys (
//	    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
//	    user_id        BIGINT      NOT NULL,
//	    idem_key       TEXT        NOT NULL,
//	    request_method TEXT        NOT NULL,
//	    request_path   TEXT        NOT NULL,
//	    request_hash   TEXT        NOT NULL,
//	    locked_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
//	    subject_id     BIGINT,          -- opaque caller reference; add an FK if desired
//	    response_code  INT,
//	    response_body  JSONB,
//	    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
//	    UNIQUE (user_id, idem_key)
//	);
package idempotency

import (
	"errors"
	"time"
)

// Claim outcomes surfaced to callers.
var (
	// ErrConflict: the same key arrived with a different request (hash/path/method).
	ErrConflict = errors.New("idempotency key reused with a different request")
	// ErrLocked: another attempt with the same key is in-flight and not yet stale.
	ErrLocked = errors.New("idempotency key locked by an in-flight request")
	// ErrNotFound: the key row does not exist.
	ErrNotFound = errors.New("idempotency key not found")
)

// Record is one claimed request: its identity, in-flight lock, the subject it
// created (checkpointed for crash-recovery re-entry), and — once finished — the
// cached response that replays verbatim.
type Record struct {
	ID            int64
	UserID        int64
	Key           string
	RequestMethod string
	RequestPath   string
	RequestHash   string
	LockedAt      time.Time
	SubjectID     *int64
	ResponseCode  *int
	ResponseBody  []byte
	CreatedAt     time.Time
}

// Finished reports whether the record holds a cached response ready to replay.
func (r *Record) Finished() bool { return r.ResponseCode != nil }
