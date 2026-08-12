package idempotency

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestRecordFinished(t *testing.T) {
	code := func(c int) *int { return &c }
	cases := []struct {
		name string
		code *int
		want bool
	}{
		{"nil response code means in-flight", nil, false},
		// The zero-value trap: a stored 0 still counts as finished — Finished
		// keys on presence, not on the value.
		{"zero response code counts as finished", code(0), true},
		{"normal response code counts as finished", code(200), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Record{ResponseCode: tc.code}
			if got := r.Finished(); got != tc.want {
				t.Errorf("Finished() = %v, want %v", got, tc.want)
			}
		})
	}
}

// The sentinels are the package's error contract: callers branch on
// errors.Is, so each must match itself and nothing else.
func TestSentinelErrorsAreDistinct(t *testing.T) {
	sentinels := map[string]error{
		"ErrConflict": ErrConflict,
		"ErrLocked":   ErrLocked,
		"ErrNotFound": ErrNotFound,
	}
	for name, err := range sentinels {
		if err == nil {
			t.Fatalf("%s is nil", name)
		}
		for otherName, other := range sentinels {
			if got := errors.Is(err, other); got != (name == otherName) {
				t.Errorf("errors.Is(%s, %s) = %v, want %v", name, otherName, got, name == otherName)
			}
		}
	}
}

func TestNew_WiresFields(t *testing.T) {
	const takeover = 42 * time.Second
	r := New(nil, takeover)
	if r == nil {
		t.Fatal("New returned nil")
	}
	if r.pool != nil {
		t.Errorf("pool = %v, want nil (stored as given)", r.pool)
	}
	if r.lockTakeover != takeover {
		t.Errorf("lockTakeover = %v, want %v", r.lockTakeover, takeover)
	}
}

// fakeRow implements pgx.Row (one method) so scan's error translation is
// testable without Postgres.
type fakeRow struct {
	err  error
	fill func(dest []any)
}

func (f fakeRow) Scan(dest ...any) error {
	if f.err != nil {
		return f.err
	}
	if f.fill != nil {
		f.fill(dest)
	}
	return nil
}

func TestScan_TranslatesNoRowsToErrNotFound(t *testing.T) {
	rec, err := scan(fakeRow{err: pgx.ErrNoRows})
	if rec != nil {
		t.Errorf("record = %v, want nil", rec)
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestScan_WrapsOtherErrors(t *testing.T) {
	boom := errors.New("connection reset")
	rec, err := scan(fakeRow{err: boom})
	if rec != nil {
		t.Errorf("record = %v, want nil", rec)
	}
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the original error wrapped", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("a generic scan error must not read as ErrNotFound: %v", err)
	}
}

// scan's dest order and the columns SQL constant must stay in lockstep or
// records corrupt silently. This pins both halves: the literal column list,
// and scan's dest-to-field mapping.
func TestScan_PopulatesFieldsInColumnOrder(t *testing.T) {
	const wantColumns = `id, user_id, idem_key, request_method, request_path, request_hash,
	locked_at, subject_id, response_code, response_body, created_at`
	if columns != wantColumns {
		t.Fatalf("columns constant changed — update scan's dest order and this test together:\n%s", columns)
	}

	now := time.Now().Truncate(time.Second)
	subject := int64(7)
	respCode := 201

	rec, err := scan(fakeRow{fill: func(dest []any) {
		*dest[0].(*int64) = 11                  // id
		*dest[1].(*string) = "user-22"          // user_id
		*dest[2].(*string) = "key-1"            // idem_key
		*dest[3].(*string) = "POST"             // request_method
		*dest[4].(*string) = "/orders"          // request_path
		*dest[5].(*string) = "abc123"           // request_hash
		*dest[6].(*time.Time) = now             // locked_at
		*dest[7].(**int64) = &subject           // subject_id
		*dest[8].(**int) = &respCode            // response_code
		*dest[9].(*[]byte) = []byte(`{"ok":1}`) // response_body
		*dest[10].(*time.Time) = now            // created_at
	}})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	if rec.ID != 11 || rec.UserID != "user-22" || rec.Key != "key-1" ||
		rec.RequestMethod != "POST" || rec.RequestPath != "/orders" ||
		rec.RequestHash != "abc123" || !rec.LockedAt.Equal(now) ||
		rec.SubjectID == nil || *rec.SubjectID != subject ||
		rec.ResponseCode == nil || *rec.ResponseCode != respCode ||
		string(rec.ResponseBody) != `{"ok":1}` || !rec.CreatedAt.Equal(now) {
		t.Errorf("scan mapped fields wrong: %+v", rec)
	}
	if !rec.Finished() {
		t.Error("record with a response code must report Finished")
	}
}
