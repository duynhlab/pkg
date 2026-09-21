package slogx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/trace"

	"github.com/duynhlab/pkg/logger/slogx"
)

type creds struct {
	User     string `json:"user"`
	Password string `json:"password"`
	Nested   struct {
		APIKey string `json:"api_key"`
	} `json:"nested"`
}

type lazySecret struct{}

func (lazySecret) LogValue() slog.Value {
	return slog.GroupValue(slog.String("client_secret", "s3cr3t"), slog.String("client_id", "abc"))
}

const red = "[REDACTED]"

func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &m); err != nil {
		t.Fatalf("not JSON: %q: %v", buf.String(), err)
	}
	return m
}

func one(t *testing.T, cfg slogx.Config, attrs ...slog.Attr) map[string]any {
	t.Helper()
	buf := &bytes.Buffer{}
	cfg.Stdout = buf
	slogx.New(cfg).Info(context.Background(), "m", attrs...)
	var m map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &m); err != nil {
		t.Fatalf("not JSON: %q: %v", buf.String(), err)
	}
	return m
}

// The deny list is matched on the NORMALIZED key and applies wherever the
// key appears: top level, inside a group, inside a map or struct value, or
// produced by a LogValuer — the places a key-only check would miss.
func TestRedact_DenyListReachesEveryShape(t *testing.T) {
	var c creds
	c.User, c.Password, c.Nested.APIKey = "alice", "hunter2", "k-123"
	r := one(t, slogx.Config{},
		slog.String("Authorization", "Bearer eyJ..."),
		slog.String("Set-Cookie", "sid=1"),
		slog.String("access_token", "tok"),
		slog.String("idTokenHint", "hint"), // "token*" stem, camelCase
		slog.String("user_password_hash", "$2a$..."),
		slog.String("pass\u200bword", "zero-width"), // zero-width char inside the key
		slog.String("ｐａｓｓｗｏｒｄ", "fullwidth"),
		slog.String("client_ip", "10.0.0.1"), // RFC forbidden request attribute
		slog.String("user_id", "a11ce000-0000-4000-8000-000000000001"),
		slog.Group("http", slog.String("cookie", "sid=1"), slog.String("route", "/orders")),
		slog.Any("headers_map", map[string]any{"X-Api-Key": "k", "Accept": "json"}),
		slog.Any("creds", c),
		slog.Any("oauth", lazySecret{}),
	)
	for _, k := range []string{"Authorization", "Set-Cookie", "access_token", "idTokenHint", "user_password_hash", "pass\u200bword", "ｐａｓｓｗｏｒｄ", "client_ip"} {
		if r[k] != red {
			t.Errorf("%q = %v, want redacted", k, r[k])
		}
	}
	if r["user_id"] != "a11ce000-0000-4000-8000-000000000001" {
		t.Errorf("user_id must survive: %v", r["user_id"])
	}
	http := r["http"].(map[string]any)
	if http["cookie"] != red || http["route"] != "/orders" {
		t.Errorf("group: %v", http)
	}
	headers := r["headers_map"].(map[string]any)
	if headers["X-Api-Key"] != red || headers["Accept"] != "json" {
		t.Errorf("map value: %v", headers)
	}
	cr := r["creds"].(map[string]any)
	if cr["password"] != red || cr["user"] != "alice" || cr["nested"].(map[string]any)["api_key"] != red {
		t.Errorf("struct value: %v", cr)
	}
	oauth := r["oauth"].(map[string]any)
	if oauth["client_secret"] != red || oauth["client_id"] != "abc" {
		t.Errorf("LogValuer output: %v", oauth)
	}
}

func TestRedact_ValuesArePANCheckedAndBounded(t *testing.T) {
	long := strings.Repeat("x", 5000) + "—" + strings.Repeat("y", 10)
	r := one(t, slogx.Config{},
		slog.String("card", "4111 1111 1111 1111"),               // Luhn-valid test PAN
		slog.String("order_ref", "4111111111111112"),             // 16 digits, fails Luhn — keep
		slog.String("snowflake", "1234567890123456789"),          // Luhn-valid but IIN 1 — keep
		slog.String("trace", "4bf92f3577b34da6a3ce929d0e0e4736"), // hex, keep
		slog.String("note", "card 4111-1111-1111-1111 declined"), // embedded PAN
		slog.String("value", long),
		slog.String("bad_utf8", "abc\xffdef"),
		slog.Any("err", errors.New("connect: "+strings.Repeat("p", 5000))),
		slog.Int("attempt", 3),
	)
	if r["card"] != red {
		t.Errorf("PAN not redacted: %v", r["card"])
	}
	if r["order_ref"] != "4111111111111112" || r["trace"] != "4bf92f3577b34da6a3ce929d0e0e4736" || r["snowflake"] != "1234567890123456789" {
		t.Errorf("non-PAN digits must survive: %v %v %v", r["order_ref"], r["trace"], r["snowflake"])
	}
	if r["note"] != "card "+red+" declined" {
		t.Errorf("embedded PAN must be replaced in place: %v", r["note"])
	}
	v := r["value"].(string)
	if len(v) > 4096+len("…(truncated)") || !strings.HasSuffix(v, "…(truncated)") || !utf8.ValidString(v) {
		t.Errorf("value not bounded rune-safely: len=%d", len(v))
	}
	if !utf8.ValidString(r["bad_utf8"].(string)) || !strings.Contains(r["bad_utf8"].(string), "�") {
		t.Errorf("invalid UTF-8 must be repaired: %q", r["bad_utf8"])
	}
	if e := r["err"].(string); !strings.HasSuffix(e, "…(truncated)") {
		t.Errorf("error messages are bounded too: len=%d", len(e))
	}
	if r["attempt"] != float64(3) {
		t.Errorf("non-string kinds pass through: %v", r["attempt"])
	}
}

func TestRedact_CountAndDepthCaps(t *testing.T) {
	attrs := make([]slog.Attr, 0, 70)
	for i := 0; i < 70; i++ {
		attrs = append(attrs, slog.Int("a"+strconv.Itoa(i), i))
	}
	r := one(t, slogx.Config{}, attrs...)
	if r["_slogx.dropped"] != float64(6) {
		t.Errorf("_slogx.dropped = %v, want 6 (70 attrs, cap 64)", r["_slogx.dropped"])
	}
	// A single map value cannot blow past the cap either: leaves inside
	// groups draw from the same budget.
	big := map[string]any{}
	for i := 0; i < 500; i++ {
		big["k"+strconv.Itoa(i)] = i
	}
	r = one(t, slogx.Config{}, slog.Any("big_map", big))
	if n := len(r["big_map"].(map[string]any)); n != 64 {
		t.Errorf("group leaves = %d, want 64 (shared budget)", n)
	}
	if r["_slogx.dropped"] != float64(436) {
		t.Errorf("_slogx.dropped = %v, want 436", r["_slogx.dropped"])
	}
	deep := slog.Group("l1", slog.Group("l2", slog.Group("l3", slog.Group("l4", slog.Group("l5", slog.String("k", "v"))))))
	r = one(t, slogx.Config{}, deep)
	l4 := r["l1"].(map[string]any)["l2"].(map[string]any)["l3"].(map[string]any)["l4"]
	if l4 != "[TRUNCATED: depth]" {
		t.Errorf("depth beyond 4 must be replaced by the depth marker, got %v", l4)
	}
}

// A logger bound once with a secret would leak it on every line for the rest
// of the process — With must redact too, and bound attributes spend the same
// budget as the record's own.
func TestRedact_AppliesToBoundAttrs(t *testing.T) {
	buf := &bytes.Buffer{}
	bound := make([]slog.Attr, 0, 60)
	for i := 0; i < 60; i++ {
		bound = append(bound, slog.Int("b"+strconv.Itoa(i), i))
	}
	log := slogx.New(slogx.Config{Stdout: buf}).With(slog.String("api_key", "k"), slog.String("component", "c")).With(bound...)
	attrs := make([]slog.Attr, 0, 10)
	for i := 0; i < 10; i++ {
		attrs = append(attrs, slog.Int("r"+strconv.Itoa(i), i))
	}
	log.Info(context.Background(), "x", attrs...)
	m := decode(t, buf)
	if m["api_key"] != red || m["component"] != "c" {
		t.Errorf("bound attrs: %v", m)
	}
	if m["_slogx.dropped"] != float64(8) {
		t.Errorf("_slogx.dropped = %v, want 8 (2+60 bound + 10 record = 72, cap 64)", m["_slogx.dropped"])
	}
}

// A service can only make the policy STRICTER: extra keys are added to the
// platform list, never substituted for it, and a bound wider than the platform
// cap is ignored. There is no off switch.
func TestRedact_PolicyCanOnlyTighten(t *testing.T) {
	r := one(t, slogx.Config{Redact: slogx.RedactPolicy{ExtraDenyKeys: []string{"employee_id"}, MaxValueLen: 1 << 30, MaxAttrs: 1 << 20}},
		slog.String("employee_id", "e1"), slog.String("password", "p"), slog.String("v", strings.Repeat("x", 5000)))
	if r["employee_id"] != red || r["password"] != red {
		t.Errorf("extra + platform keys: %v", r)
	}
	if len(r["v"].(string)) > 4096+len("…(truncated)") {
		t.Error("a bound wider than the platform cap must be ignored")
	}
	r = one(t, slogx.Config{Redact: slogx.RedactPolicy{MaxValueLen: 10}}, slog.String("v", strings.Repeat("x", 50)))
	if len(r["v"].(string)) != 10+len("…(truncated)") {
		t.Errorf("a tighter bound is honoured: %d", len(r["v"].(string)))
	}
}

// Values are scanned too: a secret can arrive under any key — in an error
// string, a URL, a header dump, a JSON body logged as text, or the message.
func TestRedact_ValueScannersAndMessage(t *testing.T) {
	buf := &bytes.Buffer{}
	log := slogx.New(slogx.Config{Stdout: buf})
	log.Info(context.Background(), "upstream said Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.abcdefghijklmnop and card 4111111111111111",
		slog.String("header", "Basic dXNlcjpwYXNzd29yZA=="),
		slog.String("url", "https://idp.example/token?grant_type=code&client_secret=cs_live_123&redirect_uri=x"),
		slog.String("dsn", "postgres://app:dbPASS@db:5432/x"),               // key is denied anyway
		slog.String("database_url_hint", "postgres://app:dbPASS@db:5432/x"), // innocent key, userinfo scanned
		slog.String("raw", `{"password":"hunter2","user":"alice"}`),
		slog.Any("err", errors.New(`dial tcp: connect to postgres://svc:pw123@host failed`)),
	)
	m := decode(t, buf)
	msg := m["message"].(string)
	if strings.Contains(msg, "eyJ") || strings.Contains(msg, "4111111111111111") || !strings.HasPrefix(msg, "upstream said Authorization: "+red) {
		t.Errorf("message not scanned: %q", msg)
	}
	if m["header"] != "Basic "+red {
		t.Errorf("basic auth: %v", m["header"])
	}
	if u := m["url"].(string); strings.Contains(u, "cs_live_123") || !strings.Contains(u, "grant_type=code") {
		t.Errorf("query credential: %q", u)
	}
	if m["dsn"] != red || m["database_url_hint"] != "postgres://app:"+red+"@db:5432/x" {
		t.Errorf("dsn/userinfo: %v / %v", m["dsn"], m["database_url_hint"])
	}
	if raw := m["raw"].(string); strings.Contains(raw, "hunter2") || !strings.Contains(raw, `"user":"alice"`) {
		t.Errorf("json-in-string: %q", raw)
	}
	if e := m["err"].(string); strings.Contains(e, "pw123") {
		t.Errorf("error text: %q", e)
	}
	buf.Reset()
	log.Info(context.Background(), strings.Repeat("m", 5000))
	m = decode(t, buf)
	if len(m["message"].(string)) > 1024+len("…(truncated)") {
		t.Error("message must be bounded")
	}
}

type nilErr struct{ msg string }

func (e *nilErr) Error() string { return e.msg } // panics on a nil receiver

type badMarshal struct{ Password string }

func (badMarshal) MarshalJSON() ([]byte, error) { return nil, errors.New("nope") }

type hidden struct {
	DB       string
	Password string `json:"-"`
	Cb       func()
}

// A typed-nil error, a panicking method or an unmarshalable struct must never
// take the process down and must never fall back to %v — that would print
// the fields the author hid with json:"-".
func TestRedact_FailsClosedAndNeverPanics(t *testing.T) {
	var e *nilErr
	r := one(t, slogx.Config{},
		slog.Any("typed_nil", e),
		slog.Any("bad", badMarshal{Password: "p"}),
		slog.Any("conn", hidden{DB: "x", Password: "hunter2", Cb: func() {}}),
		slog.Any("chan", make(chan int)),
		slog.Int64("card_no", 4111111111111111), // PAN as a number
		slog.Int64("order_id", 1234567890123456789),
	)
	if v, ok := r["typed_nil"]; !ok || v != nil {
		t.Errorf("typed nil must render as null, got %v", r["typed_nil"])
	}
	for _, k := range []string{"bad", "chan"} {
		v, _ := r[k].(string)
		if !strings.HasPrefix(v, red) || strings.Contains(v, "hunter2") {
			t.Errorf("%s must be replaced, never rendered with %%v: %v", k, r[k])
		}
	}
	// A struct is walked field by field: json:"-" hides the field, a func
	// field is replaced, the rest survive.
	conn := r["conn"].(map[string]any)
	if _, leaked := conn["Password"]; leaked || conn["DB"] != "x" || !strings.HasPrefix(conn["Cb"].(string), red) {
		t.Errorf("struct fields: %v", conn)
	}
	if r["card_no"] != red {
		t.Errorf("numeric PAN: %v", r["card_no"])
	}
	if r["order_id"] != float64(1234567890123456789) {
		t.Errorf("numeric id with a non-card IIN must survive: %v", r["order_id"])
	}
}

type stringer struct{}

func (stringer) String() string { return "token=abc" } // scanned as a value: the k=v shape is caught

func TestRedact_OtherValueShapes(t *testing.T) {
	r := one(t, slogx.Config{},
		slog.Any("nil", nil),
		slog.Any("stringer", stringer{}),
		slog.Any("bytes", []byte("raw")),
		slog.Any("list", []any{"a", true, nil, map[string]any{"password": "p"}}),
		slog.Any("flags", map[string]any{"on": true, "n": 1.5, "big": int64(1234567890123456789), "nothing": nil}),
	)
	if r["stringer"] != "token="+red || r["bytes"] != "raw" {
		t.Errorf("stringer/bytes: %v %v", r["stringer"], r["bytes"])
	}
	list := r["list"].(map[string]any)
	if list["0"] != "a" || list["1"] != true || list["3"].(map[string]any)["password"] != red {
		t.Errorf("arrays become indexed groups whose keys are checked: %v", list)
	}
	flags := r["flags"].(map[string]any)
	if flags["on"] != true || flags["n"] != 1.5 || flags["big"] != float64(1234567890123456789) {
		t.Errorf("scalars pass through with integer precision kept: %v", flags)
	}
	if _, ok := r["nil"]; !ok {
		t.Errorf("nil value is kept as null: %v", r)
	}
}

func TestRedact_GroupedLoggerAndEnabled(t *testing.T) {
	buf := &bytes.Buffer{}
	log := slogx.New(slogx.Config{Level: "warn", Stdout: buf})
	g := log.Slog().WithGroup("req")
	if g.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("the grouped bridge logger must honour the shared level gate")
	}
	g.Warn("x", "password", "p", "route", "/r")
	m := decode(t, buf)
	req := m["req"].(map[string]any)
	if req["password"] != red || req["route"] != "/r" {
		t.Errorf("redaction inside WithGroup: %v", m)
	}
}

type boom struct{}

func (boom) Error() string { panic("method exploded") }

// Once the budget is spent every kind is dropped, not partially rendered,
// and the drop counter accounts for each of them.
func TestRedact_BudgetExhaustedDropsEveryKind(t *testing.T) {
	attrs := make([]slog.Attr, 0, 80)
	for i := 0; i < 64; i++ {
		attrs = append(attrs, slog.Int("a"+strconv.Itoa(i), i))
	}
	attrs = append(attrs,
		slog.String("s", "x"),
		slog.Int64("i", 1),
		slog.Uint64("u", 1),
		slog.Float64("f", 1.5),
		slog.Bool("b", true),
		slog.String("password", "p"),
		slog.Any("e", errors.New("x")),
		slog.Any("st", stringer{}),
		slog.Any("by", []byte("x")),
		slog.Any("nil", nil),
		slog.Any("bad", badMarshal{}),
		slog.Any("boom", boom{}),
		slog.Any("m", map[string]any{"k": "v"}),
		slog.Group("g", slog.String("k", "v")),
		slog.Group("deep", slog.Group("l2", slog.Group("l3", slog.Group("l4", slog.Group("l5", slog.String("k", "v")))))),
	)
	r := one(t, slogx.Config{}, attrs...)
	for _, k := range []string{"s", "i", "u", "f", "b", "password", "e", "st", "by", "nil", "bad", "boom", "m", "g", "deep"} {
		if _, ok := r[k]; ok {
			t.Errorf("%q must be dropped once the budget is spent", k)
		}
	}
	if r["_slogx.dropped"] != float64(15) {
		t.Errorf("_slogx.dropped = %v, want 15", r["_slogx.dropped"])
	}
	// With budget left, a panicking method is caught and marked, never fatal.
	r = one(t, slogx.Config{}, slog.Any("boom", boom{}), slog.Float64("card_f", 4111111111111111), slog.Uint64("card_u", 4111111111111111))
	if v, _ := r["boom"].(string); !strings.HasPrefix(v, "!PANIC (string): method exploded") {
		t.Errorf("panic marker: %v", r["boom"])
	}
	if r["card_f"] != red || r["card_u"] != red {
		t.Errorf("PAN in float/uint kinds: %v %v", r["card_f"], r["card_u"])
	}
}

func TestDefaultRedactPolicy_IsThePlatformBound(t *testing.T) {
	p := slogx.DefaultRedactPolicy()
	if p.MaxAttrs != 64 || p.MaxDepth != 4 || p.MaxValueLen != 4096 || p.MaxMessageLen != 1024 || len(p.ExtraDenyKeys) != 0 {
		t.Errorf("unexpected default policy: %+v", p)
	}
}

// Value scanning, exact input → output. Positives are the shapes the
// security audits produced; negatives are the platform's own strings that a
// greedy scanner would mangle (gRPC status, HTTP status, timestamps, ids).
func TestRedact_ScanTable(t *testing.T) {
	cases := []struct{ in, want string }{
		// deny-listed keys inside text — same list as attribute keys
		{"X-Auth-Token: SECRET_A", "X-Auth-Token: " + red},
		{"X-Api-Key: SECRET_B", "X-Api-Key: " + red},
		{"POSTGRES_PASSWORD=SECRET_C", "POSTGRES_PASSWORD=" + red},
		{`{"user_password":"SECRET_D"}`, `{"user_password":"` + red + `"}`},
		{`{"accessToken":"SECRET_E","refreshToken":"SECRET_F"}`, `{"accessToken":"` + red + `","refreshToken":"` + red + `"}`},
		{`{"clientSecret":"SECRET_G"}`, `{"clientSecret":"` + red + `"}`},
		{"map[X-Auth-Token:[SECRET_H] Accept:[json]]", "map[X-Auth-Token:" + red + " Accept:[json]]"},
		{`{"credentials":{"user":"a","pass":"S23"},"ok":1}`, `{"credentials":` + red + `,"ok":1}`},
		{`{"tokens":["S24","S25"],"n":2}`, `{"tokens":` + red + `,"n":2}`},
		{"headers: map[X-Custom:[S29] Accept:[json]] next=1", "headers: " + red + " next=1"},
		{"Cookie: sid=S26; theme=dark", "Cookie: " + red},
		{"Set-Cookie: Path=/; sid=S28; HttpOnly", "Set-Cookie: " + red},
		{"&{Tokens:[S30 S31] N:2}", "&{Tokens:" + red + " N:2}"},
		{"password=\tS9 next", "password=\t" + red + " next"},
		{"password\t=\tS10", "password\t=\t" + red},
		{strings.Repeat("x", 130) + "_password=S20", strings.Repeat("x", 130) + "_password=" + red},
		{"ｐａｓｓｗｏｒｄ=SECRET_FW", "ｐａｓｓｗｏｒｄ=" + red},
		// card numbers next to other digit groups, glued to letters, two in a row
		{"card 4111111111111111 12/26 declined", "card " + red + " 12/26 declined"},
		{"card 4111 1111 1111 1111 123", "card " + red + " 123"},
		{"card 4111111111111111-2026", "card " + red + "-2026"},
		{"4111111111111111 0", red + " 0"},
		{"4111111111111111 and 5500000000000004", red + " and " + red},
		{"x 12345 4111111111111111 9", "x 12345 " + red + " 9"},
		{"4111_1111_1111_1111", red},
		{"ref 4111111111111111abc", "ref 4111111111111111abc"},
		{"ref abc4111111111111111", "ref abc4111111111111111"},
		{"ref id_4111111111111111", "ref id_4111111111111111"},
		// prose after a scheme word is not a credential
		{"basic validation failed", "basic validation failed"},
		{"Basic configuration loaded", "Basic configuration loaded"},
		{"Bearer authentication required", "Bearer authentication required"},
		{`password="correct horse battery staple"`, `password="` + red + `"`},
		{`{"password":"a,b,c","user":"x"}`, `{"password":"` + red + `","user":"x"}`},
		{`{"password":"a\"b","user":"x"}`, `{"password":"` + red + `","user":"x"}`},
		{"Authorization: Token ghp_SECRETTAIL", "Authorization: " + red},
		{"Authorization: Bearer abc1234", "Authorization: " + red},
		{`Authorization: Digest username="alice", response="SECRET"`, "Authorization: " + red},
		{"client_secret=cs_live_123&grant_type=code", "client_secret=" + red + "&grant_type=code"},
		{"token=abc", "token=" + red},
		// bearer / basic / jwt
		{"Basic dXNlcjpwYXNzd29yZA==", "Basic " + red},
		{"got eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.abcdefghijklmnop back", "got " + red + " back"},
		// url userinfo, empty user included; a port before an "@" in the path is not one
		{"postgres://app:SECRET@db:5432/x", "postgres://app:" + red + "@db:5432/x"},
		{"redis://:SECRET_R@valkey:6379/1", "redis://:" + red + "@valkey:6379/1"},
		{"rediss://:SECRET_R2@valkey:6379", "rediss://:" + red + "@valkey:6379"},
		{"https://api.example.com:443/users/alice@example.com", "https://api.example.com:443/users/alice@example.com"},
		// card numbers: Luhn + IIN gate, in place
		{"card 4111-1111-1111-1111 declined", "card " + red + " declined"},
		{"order 4111111111111112", "order 4111111111111112"},
		{"snowflake 1234567890123456789", "snowflake 1234567890123456789"},
		{"nanos 1789959127000000000", "nanos 1789959127000000000"},
		{"id a11ce000-0000-4000-8000-000000000001", "id a11ce000-0000-4000-8000-000000000001"},
		// platform strings that must survive
		{"rpc error: code = NotFound desc = no such order", "rpc error: code = NotFound desc = no such order"},
		{"status code: 500", "status code: 500"},
		{"exit code=1", "exit code=1"},
		{"http.status_code=503 http.route=/orders", "http.status_code=503 http.route=/orders"},
		{"pwd: /home/alice/app", "pwd: /home/alice/app"},
		{"2026-09-21T02:51:36Z time=12:30:00", "2026-09-21T02:51:36Z time=12:30:00"},
		{"peer.service=inventory http.request.body.size=120", "peer.service=inventory http.request.body.size=120"},
		{"user_id: 42 route: /orders", "user_id: 42 route: /orders"},
		{"", ""},
	}
	buf := &bytes.Buffer{}
	log := slogx.New(slogx.Config{Stdout: buf})
	for _, c := range cases {
		buf.Reset()
		log.Info(context.Background(), c.in, slog.String("v", c.in))
		var m map[string]any
		if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &m); err != nil {
			t.Fatalf("%q: not JSON: %v", c.in, err)
		}
		if m["v"] != c.want {
			t.Errorf("value %q\n got %q\nwant %q", c.in, m["v"], c.want)
		}
		if c.in != "" && m["message"] != c.want {
			t.Errorf("message %q\n got %q\nwant %q", c.in, m["message"], c.want)
		}
	}
}

func TestRedact_WithOverflowIsCounted(t *testing.T) {
	buf := &bytes.Buffer{}
	bound := make([]slog.Attr, 0, 70)
	for i := 0; i < 70; i++ {
		bound = append(bound, slog.Int("b"+strconv.Itoa(i), i))
	}
	slogx.New(slogx.Config{Stdout: buf}).With(bound...).Info(context.Background(), "x")
	m := decode(t, buf)
	if m["_slogx.dropped"] != float64(6) {
		t.Errorf("attrs dropped at bind time must be reported: %v", m["_slogx.dropped"])
	}
}

// The envelope's own attributes are added below the redactor: a record at
// the attribute cap keeps its correlation, and an all-digit span id is not a
// card number.
func TestRedact_EnvelopeIsNotBudgetedOrScanned(t *testing.T) {
	tid, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	sid, _ := trace.SpanIDFromHex("4111111111111111")
	ctx := trace.ContextWithSpanContext(context.Background(),
		trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled}))
	attrs := make([]slog.Attr, 0, 64)
	for i := 0; i < 64; i++ {
		attrs = append(attrs, slog.Int("a"+strconv.Itoa(i), i))
	}
	buf := &bytes.Buffer{}
	slogx.New(slogx.Config{Stdout: buf}).Info(ctx, "x", attrs...)
	m := decode(t, buf)
	if m["trace_id"] != "4bf92f3577b34da6a3ce929d0e0e4736" || m["span_id"] != "4111111111111111" {
		t.Errorf("trace ids must survive a full record untouched: %v %v", m["trace_id"], m["span_id"])
	}
	if _, ok := m["_slogx.dropped"]; ok {
		t.Errorf("64 attrs fit exactly; nothing may be dropped: %v", m["_slogx.dropped"])
	}
}

func TestRedact_PolicyStemsAndBounds(t *testing.T) {
	// A stem that normalizes to nothing is ignored, a real stem works.
	r := one(t, slogx.Config{Redact: slogx.RedactPolicy{ExtraDenyKeys: []string{"*", "_*", "employee*"}}},
		slog.String("user_id", "u1"), slog.String("route", "/r"), slog.String("employeeNumber", "e1"))
	if r["user_id"] != "u1" || r["route"] != "/r" || r["employeeNumber"] != red {
		t.Errorf("empty stem must not match everything: %v", r)
	}
	// Tighter caps are honoured.
	attrs := make([]slog.Attr, 0, 12)
	for i := 0; i < 12; i++ {
		attrs = append(attrs, slog.Int("a"+strconv.Itoa(i), i))
	}
	r = one(t, slogx.Config{Redact: slogx.RedactPolicy{MaxAttrs: 10}}, attrs...)
	if r["_slogx.dropped"] != float64(2) {
		t.Errorf("MaxAttrs=10: dropped %v, want 2", r["_slogx.dropped"])
	}
	r = one(t, slogx.Config{Redact: slogx.RedactPolicy{MaxDepth: 2}}, slog.Group("l1", slog.Group("l2", slog.String("k", "v"))))
	if r["l1"].(map[string]any)["l2"] != "[TRUNCATED: depth]" {
		t.Errorf("MaxDepth=2: %v", r["l1"])
	}
	buf := &bytes.Buffer{}
	slogx.New(slogx.Config{Stdout: buf, Redact: slogx.RedactPolicy{MaxMessageLen: 10}}).Info(context.Background(), strings.Repeat("m", 50))
	m := decode(t, buf)
	if m["message"] != strings.Repeat("m", 10)+"…(truncated)" {
		t.Errorf("MaxMessageLen=10: %q", m["message"])
	}
}

func TestRedact_DeterministicOutput(t *testing.T) {
	v := map[string]any{"z": 1, "a": []any{"x", map[string]any{"q": true, "b": nil}}, "m": map[string]int{"k2": 2, "k1": 1}}
	var lines [2]string
	for i := range lines {
		buf := &bytes.Buffer{}
		slogx.New(slogx.Config{Stdout: buf, NoSource: true}).Info(context.Background(), "x", slog.Any("v", v))
		// strip the timestamp before comparing
		m := decode(t, buf)
		delete(m, "timestamp")
		b, _ := json.Marshal(m)
		lines[i] = string(b)
	}
	if lines[0] != lines[1] {
		t.Errorf("same value, different bytes:\n%s\n%s", lines[0], lines[1])
	}
}

type panicWithSecret struct{}

func (panicWithSecret) Error() string {
	panic("dial postgres://app:SECRET_PANIC@db failed; token=SECRET_T2")
}

type panicWithStruct struct{}

func (panicWithStruct) Error() string { panic(hidden{DB: "x", Password: "SECRET_HIDDEN"}) }

type panicInPanic struct{}

func (panicInPanic) Error() string { panic(panicWithSecret{}) } // an error whose Error() panics again

func TestRedact_PanicPayloadIsNotRendered(t *testing.T) {
	r := one(t, slogx.Config{},
		slog.Any("a", panicWithSecret{}),
		slog.Any("b", panicWithStruct{}),
		slog.Any("c", panicInPanic{}),
	)
	a, _ := r["a"].(string)
	if strings.Contains(a, "SECRET") || !strings.HasPrefix(a, "!PANIC (string): dial postgres://app:"+red+"@db failed; token="+red) {
		t.Errorf("panic text must be scanned: %q", a)
	}
	if b, _ := r["b"].(string); b != "!PANIC (slogx_test.hidden)" {
		t.Errorf("a non-text panic value must not be rendered: %q", b)
	}
	if c, _ := r["c"].(string); !strings.HasPrefix(c, "!PANIC (slogx_test.panicWithSecret): error whose Error() panics") {
		t.Errorf("nested panic: %q", c)
	}
}

type tok struct{ Raw string }

func (tok) LogValue() slog.Value { return slog.StringValue("[hidden by LogValue]") }

// A LogValuer, error or Stringer element of a slice or map is honoured
// instead of being marshalled field by field.
func TestRedact_CollectionsHonourElementMethods(t *testing.T) {
	u, _ := url.Parse("redis://:SECRET_URL@valkey:6379")
	r := one(t, slogx.Config{},
		slog.Any("top", tok{Raw: "SECRET_TOP"}),
		slog.Any("list", []tok{{Raw: "SECRET_LIST"}}),
		slog.Any("map", map[string]tok{"k": {Raw: "SECRET_MAP"}}),
		slog.Any("errs", []error{errors.New("token=SECRET_E")}),
		slog.Any("url", u),
		slog.Any("urls", []*url.URL{u}),
		slog.Any("empty", []string{}),
		slog.Any("ints", [2]int{7, 8}),
	)
	out, _ := json.Marshal(r)
	if strings.Contains(string(out), "SECRET") {
		t.Errorf("a secret survived: %s", out)
	}
	if r["top"] != "[hidden by LogValue]" || r["list"].(map[string]any)["0"] != "[hidden by LogValue]" || r["map"].(map[string]any)["k"] != "[hidden by LogValue]" {
		t.Errorf("LogValue not honoured: %v %v %v", r["top"], r["list"], r["map"])
	}
	if r["url"] != "redis://:"+red+"@valkey:6379" {
		t.Errorf("url: %v", r["url"])
	}
	if _, ok := r["empty"]; ok {
		t.Errorf("an empty collection is dropped like an empty group: %v", r["empty"])
	}
	if r["ints"].(map[string]any)["1"] != float64(8) {
		t.Errorf("array: %v", r["ints"])
	}
	r = one(t, slogx.Config{},
		slog.Any("rawmsg", json.RawMessage(`{"password":"S_RAW1","u":"x"}`)),
		slog.Any("named", namedBytes("password=S_RAW2")),
		slog.Any("runes", []rune("token=S_RAW3")),
		slog.Any("arr", [4]byte{'p', 'i', 'n', ':'}),
		slog.Any("field", withTok{T: tok{Raw: "S_STRUCTTOK"}, L: []tok{{Raw: "S_STRUCTL"}}, E: errors.New("dsn=S_ERR"), When: time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC), Skip: "S_SKIP"}),
		slog.Any("nilvaluer", (*tok)(nil)),
		slog.Any("nilvaluers", []*tok{nil}),
	)
	out, _ = json.Marshal(r)
	if strings.Contains(string(out), "S_") {
		t.Errorf("a secret survived: %s", out)
	}
	if r["rawmsg"] != `{"password":"`+red+`","u":"x"}` || r["named"] != "password="+red {
		t.Errorf("named byte slices are text: %v %v", r["rawmsg"], r["named"])
	}
	f := r["field"].(map[string]any)
	if f["T"] != "[hidden by LogValue]" || f["L"].(map[string]any)["0"] != "[hidden by LogValue]" || f["E"] != "dsn="+red || f["When"] != "2026-09-21T00:00:00Z" {
		t.Errorf("struct field methods honoured, time marshals itself: %v", f)
	}
	if _, skipped := f["Skip"]; skipped {
		t.Errorf("json:\"-\" must hide the field: %v", f)
	}
	if v, ok := r["nilvaluer"]; !ok || v != nil || r["nilvaluers"].(map[string]any)["0"] != nil {
		t.Errorf("nil LogValuer renders null, not slog's stack trace: %v %v", r["nilvaluer"], r["nilvaluers"])
	}
	if strings.Contains(string(out), "redact.go") {
		t.Errorf("build paths leaked: %s", out)
	}
}

type namedBytes []byte

type withTok struct {
	T    tok
	L    []tok
	E    error
	When time.Time
	Skip string `json:"-"`
}

func TestRedact_KeysAreBoundedAndScanned(t *testing.T) {
	long := strings.Repeat("k", 500)
	r := one(t, slogx.Config{},
		slog.String(long, "v"),
		slog.String("Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.SECRETSIGNATURE", "v"),
		slog.String("_slogx.dropped", "forged"),
		slog.String("ok", "v"),
	)
	out, _ := json.Marshal(r)
	if strings.Contains(string(out), "SECRETSIGNATURE") || strings.Contains(string(out), "forged") {
		t.Errorf("credential-shaped or reserved keys must be dropped: %s", out)
	}
	if _, ok := r[strings.Repeat("k", 128)+"…(truncated)"]; !ok {
		t.Errorf("long key must be bounded: %v", r)
	}
	if r["_slogx.dropped"] != float64(2) || r["ok"] != "v" {
		t.Errorf("dropped keys are counted: %v", r)
	}
	// group names too
	buf := &bytes.Buffer{}
	slogx.New(slogx.Config{Stdout: buf}).Slog().WithGroup("Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.SECRETSIG2").Info("x", "k", "v")
	if strings.Contains(buf.String(), "SECRETSIG2") {
		t.Errorf("group name leaked: %s", buf.String())
	}
}

// A typical service record: eight scalar attributes, no secrets. Every log
// line pays this.
func BenchmarkHandle8Attrs(b *testing.B) {
	log := slogx.New(slogx.Config{Stdout: io.Discard, NoSource: true})
	attrs := []slog.Attr{
		slog.String("http.request.method", "GET"), slog.String("http.route", "/orders/{id}"),
		slog.Int("http.response.status_code", 200), slog.String("user_id", "a11ce000-0000-4000-8000-000000000001"),
		slog.Duration("duration", 1500000), slog.String("outcome", "ok"),
		slog.String("err", "rpc error: code = NotFound desc = order 42 not found"), slog.Int64("order_id", 42),
	}
	ctx := context.Background()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		log.Info(ctx, "request completed", attrs...)
	}
}

func BenchmarkScan4KiBNoMatch(b *testing.B) {
	log := slogx.New(slogx.Config{Stdout: io.Discard, NoSource: true})
	v := strings.Repeat("the quick brown fox jumps over 12 lazy dogs at 10:30; ", 80)
	ctx := context.Background()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		log.Info(ctx, "x", slog.String("v", v))
	}
}

type withNested struct {
	Name  string           `json:"name"`
	Items []map[string]any `json:"items"`
	Score float64          `json:"score"`
	Flag  bool             `json:"flag"`
	None  *int             `json:"none"`
}

// Oversized collections are replaced, never partially walked; nested arrays
// inside a struct still have their keys checked.
func TestRedact_CollectionLimitsAndNestedStructArrays(t *testing.T) {
	big := make([]int, 5000)
	bigMap := map[string]int{}
	for i := 0; i < 5000; i++ {
		bigMap["k"+strconv.Itoa(i)] = i
	}
	r := one(t, slogx.Config{},
		slog.Any("big", big),
		slog.Any("bigmap", bigMap),
		slog.Group("l1", slog.Group("l2", slog.Group("l3", slog.Any("deep_list", []int{1}), slog.Any("deep_map", map[string]int{"a": 1})))),
		slog.Any("s", withNested{Name: "n", Items: []map[string]any{{"password": "p", "sku": "x"}}, Score: 1.5, Flag: true}),
	)
	if r["big"] != red+" (collection of 5000 elements)" || r["bigmap"] != red+" (collection of 5000 elements)" {
		t.Errorf("oversized collections: %v %v", r["big"], r["bigmap"])
	}
	l3 := r["l1"].(map[string]any)["l2"].(map[string]any)["l3"].(map[string]any)
	if l3["deep_list"] != "[TRUNCATED: depth]" || l3["deep_map"] != "[TRUNCATED: depth]" {
		t.Errorf("collections respect the depth cap: %v", l3)
	}
	s := r["s"].(map[string]any)
	item := s["items"].(map[string]any)["0"].(map[string]any)
	if item["password"] != red || item["sku"] != "x" || s["score"] != 1.5 || s["flag"] != true {
		t.Errorf("struct with nested array: %v", s)
	}
	if v, ok := s["none"]; !ok || v != nil {
		t.Errorf("null field kept as null: %v", s)
	}
}

type selfMarshal struct{ hidden string }

func (m selfMarshal) MarshalJSON() ([]byte, error) {
	return []byte(`{"password":"` + m.hidden + `","n":[1,2.5,"x"],"b":true,"none":null,"big":1234567890123456789}`), nil
}

type base struct {
	ID     string `json:"id"`
	APIKey string `json:"api_key"`
}

type embedded struct {
	base
	*extra
	Name string `json:"name,omitempty"`
	Note string `json:"-"`
}

type extra struct{ Region string }

// A type that marshals itself is walked through its own JSON; anonymous
// struct fields are flattened as encoding/json flattens them; an empty-key
// group is inlined at the parent's depth and an empty-key scalar is dropped
// and counted.
func TestRedact_MarshalerEmbeddedAndEmptyKeys(t *testing.T) {
	r := one(t, slogx.Config{},
		slog.Any("self", selfMarshal{hidden: "S_SELF"}),
		slog.Any("emb", embedded{base: base{ID: "i1", APIKey: "S_EMB"}, extra: &extra{Region: "eu"}, Name: "n", Note: "S_NOTE"}),
		slog.Any("embnil", embedded{}),
		slog.Group("", slog.String("inlined", "v"), slog.Group("l2", slog.Group("l3", slog.Group("l4", slog.String("deep", "v"))))),
		slog.String("", "orphan"),
	)
	out, _ := json.Marshal(r)
	if strings.Contains(string(out), "S_") || strings.Contains(string(out), "orphan") {
		t.Errorf("leak: %s", out)
	}
	self := r["self"].(map[string]any)
	if self["password"] != red || self["n"].(map[string]any)["1"] != 2.5 || self["b"] != true || self["big"] != float64(1234567890123456789) {
		t.Errorf("marshaler walked through JSON: %v", self)
	}
	if v, ok := self["none"]; !ok || v != nil {
		t.Errorf("null kept: %v", self)
	}
	emb := r["emb"].(map[string]any)
	if emb["id"] != "i1" || emb["api_key"] != red || emb["Region"] != "eu" || emb["name"] != "n" {
		t.Errorf("embedded fields flattened: %v", emb)
	}
	if _, ok := r["embnil"].(map[string]any)["id"]; !ok {
		t.Errorf("nil embedded pointer is skipped, the rest kept: %v", r["embnil"])
	}
	// slog.Group("") inlines: "inlined" is a top-level key and l4 sits at depth 4
	if r["inlined"] != "v" || r["l2"].(map[string]any)["l3"].(map[string]any)["l4"].(map[string]any)["deep"] != "v" {
		t.Errorf("inline group depth: %v %v", r["inlined"], r["l2"])
	}
	if r["_slogx.dropped"] != float64(1) {
		t.Errorf("empty-key scalar is dropped and counted: %v", r["_slogx.dropped"])
	}
}
