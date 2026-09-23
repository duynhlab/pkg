package httpmw_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/duynhlab/pkg/httpmw"
)

// capture is a slog.Handler that keeps every record and the context it was
// written with, so a test can assert both the attributes and the correlation.
type capture struct {
	mu   sync.Mutex
	recs []slog.Record
	ctxs []context.Context
}

func (h *capture) Enabled(context.Context, slog.Level) bool { return true }
func (h *capture) Handle(ctx context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, r.Clone())
	h.ctxs = append(h.ctxs, ctx)
	return nil
}
func (h *capture) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capture) WithGroup(string) slog.Handler      { return h }

func (h *capture) len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.recs)
}

func (h *capture) last(t *testing.T) (slog.Record, map[string]slog.Value, context.Context) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.recs) == 0 {
		t.Fatal("no record")
	}
	r := h.recs[len(h.recs)-1]
	attrs := map[string]slog.Value{}
	r.Attrs(func(a slog.Attr) bool { attrs[a.Key] = a.Value; return true })
	return r, attrs, h.ctxs[len(h.ctxs)-1]
}

func logRouter(t *testing.T, extra ...string) (*gin.Engine, *capture) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := &capture{}
	r := gin.New()
	r.Use(httpmw.Logging(slog.New(h), extra...))
	r.GET("/health", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.GET("/boom", func(c *gin.Context) { c.Status(http.StatusInternalServerError) })
	r.GET("/missing", func(c *gin.Context) { c.Status(http.StatusNotFound) })
	r.GET("/products/:id", func(c *gin.Context) { c.Status(http.StatusOK) })
	return r, h
}

func get(t *testing.T, r *gin.Engine, path string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	r.ServeHTTP(rec, req)
	return rec
}

// Logging and Tracing must exclude the same routes — that is the whole reason
// the skip list lives in one map. A successful probe is silent; a real request
// is logged once.
func TestLogging_SkipsSuccessfulProbesOnly(t *testing.T) {
	r, h := logRouter(t)
	get(t, r, "/health")
	if n := h.len(); n != 0 {
		t.Errorf("records = %d, want 0 for a successful probe", n)
	}
	get(t, r, "/products/42")
	if n := h.len(); n != 1 {
		t.Errorf("records = %d, want exactly 1 for a real request", n)
	}
}

// A failing probe is the one time a probe is worth reading, so the skip list
// must not swallow it.
func TestLogging_KeepsFailingProbes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &capture{}
	r := gin.New()
	r.Use(httpmw.Logging(slog.New(h)))
	r.GET("/ready", func(c *gin.Context) { c.Status(http.StatusServiceUnavailable) })
	get(t, r, "/ready")
	rec, _, _ := h.last(t)
	if rec.Level != slog.LevelError {
		t.Errorf("failing probe level = %v, want Error", rec.Level)
	}
}

// The access-outcome severity: Error for a server failure, Warn for a client
// error, Info otherwise.
func TestLogging_LevelFollowsStatusClass(t *testing.T) {
	for _, tc := range []struct {
		path string
		want slog.Level
	}{
		{"/products/1", slog.LevelInfo},
		{"/missing", slog.LevelWarn},
		{"/boom", slog.LevelError},
	} {
		r, h := logRouter(t)
		get(t, r, tc.path)
		rec, _, _ := h.last(t)
		if rec.Level != tc.want {
			t.Errorf("%s: level = %v, want %v", tc.path, rec.Level, tc.want)
		}
	}
}

// The record carries exactly the canonical attributes: the matched route
// template, never the raw path; the status code; error.type only for a server
// failure. RFC-0031 § Canonical attributes.
func TestLogging_CanonicalAttributesOnly(t *testing.T) {
	r, h := logRouter(t)
	get(t, r, "/products/42?token=secret", "User-Agent", "curl/8.5.0", "X-Forwarded-For", "203.0.113.9")
	rec, a, _ := h.last(t)
	if rec.Message != "HTTP request" {
		t.Errorf("message = %q", rec.Message)
	}
	if a["http.request.method"].String() != "GET" || a["http.route"].String() != "/products/:id" || a["http.response.status_code"].Int64() != 200 {
		t.Errorf("canonical attributes: %v", a)
	}
	if _, ok := a["error.type"]; ok {
		t.Errorf("error.type must be absent on success: %v", a)
	}
	for _, forbidden := range []string{"path", "url.path", "url.query", "status", "method", "duration",
		"client_ip", "client.address", "user_agent", "user_agent.original", "trace_id"} {
		if _, ok := a[forbidden]; ok {
			t.Errorf("forbidden attribute %q present: %v", forbidden, a)
		}
	}
	if len(a) != 3 {
		t.Errorf("want exactly three attributes on a success, got %d: %v", len(a), a)
	}

	get(t, r, "/boom")
	_, a, _ = h.last(t)
	if a["error.type"].String() != "500" {
		t.Errorf("a server failure names its status as error.type: %v", a)
	}
	get(t, r, "/missing")
	_, a, _ = h.last(t)
	if _, ok := a["error.type"]; ok {
		t.Errorf("a client error is not a server failure: %v", a)
	}
}

// An unmatched path has no route template; the attribute is omitted rather
// than filled with the raw path, which would put an unbounded, attacker-chosen
// value into a low-cardinality field.
func TestLogging_UnmatchedRouteOmitsTheRoute(t *testing.T) {
	r, h := logRouter(t)
	get(t, r, "/no/such/path/4111111111111111")
	_, a, _ := h.last(t)
	if _, ok := a["http.route"]; ok {
		t.Errorf("http.route must be absent when nothing matched: %v", a)
	}
	if a["http.response.status_code"].Int64() != 404 {
		t.Errorf("status: %v", a)
	}
}

// The record is written with the request context, so a context-aware handler
// can take the trace and span ids from the span — the only ids that may reach
// telemetry. The response header still carries an id for the client.
func TestLogging_CorrelatesThroughTheContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	h := &capture{}
	r := gin.New()
	var spanTrace string
	r.Use(func(c *gin.Context) {
		ctx, span := tp.Tracer("t").Start(c.Request.Context(), "req")
		defer span.End()
		spanTrace = span.SpanContext().TraceID().String()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	r.Use(httpmw.Logging(slog.New(h)))
	r.GET("/x", func(c *gin.Context) { c.Status(http.StatusOK) })

	rec := get(t, r, "/x")
	_, _, ctx := h.last(t)
	sc := oteltrace.SpanContextFromContext(ctx)
	if sc.TraceID().String() != spanTrace {
		t.Errorf("record context must carry the request span: got %s want %s", sc.TraceID(), spanTrace)
	}
	if rec.Header().Get(httpmw.TraceIDHeader) != spanTrace {
		t.Errorf("header = %q, want the span's trace id", rec.Header().Get(httpmw.TraceIDHeader))
	}
}

// Without a span the header falls back to a generated id — a client contract —
// but that id must never become a trace id in telemetry: the record's context
// has no span.
func TestLogging_HeaderAlwaysSetButGeneratedIDNeverLogged(t *testing.T) {
	r, h := logRouter(t)
	rec := get(t, r, "/products/1")
	if len(rec.Header().Get(httpmw.TraceIDHeader)) != 32 {
		t.Errorf("header must carry a generated id: %q", rec.Header().Get(httpmw.TraceIDHeader))
	}
	_, a, ctx := h.last(t)
	if oteltrace.SpanContextFromContext(ctx).IsValid() {
		t.Error("no span existed, so the record context must carry none")
	}
	if _, ok := a["trace_id"]; ok {
		t.Error("the generated id must never be logged")
	}
}

func TestTraceID_FallbackOrder(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const want = "4bf92f3577b34da6a3ce929d0e0e4736"
	const other = "0af7651916cd43dd8448eb211c80319c"

	cases := []struct {
		name        string
		traceparent string
		xTraceID    string
		want        string // empty: a freshly generated id
	}{
		{name: "traceparent", traceparent: "00-" + want + "-00f067aa0ba902b7-01", xTraceID: other, want: want},
		{name: "x-trace-id", xTraceID: other, want: other},
		{name: "malformed traceparent falls through", traceparent: "garbage", xTraceID: other, want: other},
		{name: "all-zero traceparent falls through", traceparent: "00-00000000000000000000000000000000-00f067aa0ba902b7-01", xTraceID: other, want: other},
		{name: "arbitrary X-Trace-ID is not echoed", xTraceID: "<script>alert(1)</script>"},
		{name: "uppercase X-Trace-ID is not echoed", xTraceID: "4BF92F3577B34DA6A3CE929D0E0E4736"},
		{name: "generated when nothing is present"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.traceparent != "" {
				c.Request.Header.Set(httpmw.TraceParentHeader, tc.traceparent)
			}
			if tc.xTraceID != "" {
				c.Request.Header.Set(httpmw.TraceIDHeader, tc.xTraceID)
			}
			got := httpmw.TraceID(c)
			if tc.want != "" {
				if got != tc.want {
					t.Errorf("TraceID = %q, want %q", got, tc.want)
				}
				return
			}
			if got == tc.xTraceID || !isHex32(got) {
				t.Errorf("TraceID = %q, want a generated 32-hex id", got)
			}
		})
	}
}

func isHex32(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// A method outside the standard nine is a value the client chose; it is logged
// as "_OTHER", the value the span carries, never verbatim.
func TestLogging_NonStandardMethodIsOther(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &capture{}
	r := gin.New()
	r.Use(httpmw.Logging(slog.New(h)))
	r.Handle("PROPFIND", "/x", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.GET("/x", func(c *gin.Context) { c.Status(http.StatusOK) })

	for method, want := range map[string]string{"PROPFIND": "_OTHER", http.MethodGet: http.MethodGet} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(method, "/x", nil))
		_, a, _ := h.last(t)
		if got := a["http.request.method"].String(); got != want {
			t.Errorf("%s: http.request.method = %q, want %q", method, got, want)
		}
	}
}

// LoggerFrom writes through the logger Logging was given, bound to the request:
// a call with no context of its own still carries the request span, and a call
// that passes a span of its own keeps that one. Without Logging it is silent —
// a second, uncorrelated logger would hide that mistake; silence surfaces it.
type callerKey struct{}

func TestLoggerFrom(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	tracer := tp.Tracer("test")

	h := &capture{}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		ctx, span := tracer.Start(c.Request.Context(), "req")
		defer span.End()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	r.Use(httpmw.Logging(slog.New(h)))
	var reqSpan, childSpan oteltrace.SpanContext
	var ctxs []context.Context
	r.GET("/x", func(c *gin.Context) {
		reqSpan = oteltrace.SpanContextFromContext(c.Request.Context())
		l := httpmw.LoggerFrom(c).With("k", "v")
		l.Info("no context")
		ctxs = append(ctxs, h.ctxs[len(h.ctxs)-1])

		child, span := tracer.Start(c.Request.Context(), "child")
		childSpan = span.SpanContext()
		l.InfoContext(child, "own span")
		span.End()
		ctxs = append(ctxs, h.ctxs[len(h.ctxs)-1])

		l.InfoContext(context.WithValue(context.Background(), callerKey{}, "kept"), "spanless context")
		ctxs = append(ctxs, h.ctxs[len(h.ctxs)-1])
		c.Status(http.StatusOK)
	})
	get(t, r, "/x")

	for i, want := range []oteltrace.SpanContext{reqSpan, childSpan, reqSpan} {
		if got := oteltrace.SpanContextFromContext(ctxs[i]); got.SpanID() != want.SpanID() {
			t.Errorf("record %d: span = %s, want %s", i, got.SpanID(), want.SpanID())
		}
	}
	if ctxs[2].Value(callerKey{}) != "kept" {
		t.Error("a spanless caller context must keep its own values; only the span is added")
	}
	if childSpan.SpanID() == reqSpan.SpanID() {
		t.Fatal("test setup: the child span must differ from the request span")
	}

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	silent := httpmw.LoggerFrom(c)
	if silent == nil {
		t.Fatal("without Logging, LoggerFrom must return a silent logger")
	}
	before := h.len()
	silent.Error("must go nowhere")
	if h.len() != before {
		t.Error("the fallback logger must not write anywhere")
	}
}

// A nil logger must not panic the middleware; it logs nowhere.
func TestLogging_NilLoggerIsSilent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(httpmw.Logging(nil))
	r.GET("/x", func(c *gin.Context) { c.Status(http.StatusInternalServerError) })
	if rec := get(t, r, "/x"); rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestSkipper_ExtraRoutesAddToDefaults(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &capture{}
	r := gin.New()
	r.Use(httpmw.Logging(slog.New(h), "/internal/debug"))
	r.GET("/internal/debug", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.GET("/health", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.GET("/x", func(c *gin.Context) { c.Status(http.StatusOK) })

	get(t, r, "/internal/debug")
	get(t, r, "/health")
	if h.len() != 0 {
		t.Errorf("records = %d, want 0 — extras and defaults must both skip", h.len())
	}
	get(t, r, "/x")
	if h.len() != 1 {
		t.Errorf("records = %d, want 1 — a normal route must still log", h.len())
	}
}
