package httpmw_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/duynhlab/pkg/httpmw"
)

func logRouter(t *testing.T) (*gin.Engine, *observer.ObservedLogs) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	core, logs := observer.New(zapcore.DebugLevel)

	r := gin.New()
	r.Use(httpmw.Logging(zap.New(core)))
	r.GET("/health", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.GET("/boom", func(c *gin.Context) { c.Status(http.StatusInternalServerError) })
	r.GET("/missing", func(c *gin.Context) { c.Status(http.StatusNotFound) })
	r.GET("/products/:id", func(c *gin.Context) { c.Status(http.StatusOK) })
	return r, logs
}

func get(t *testing.T, r *gin.Engine, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// Logging and Tracing must exclude the same routes — that is the whole reason
// the skip list lives in one map. A successful probe is silent; a real request
// is logged.
func TestLogging_SkipsSuccessfulProbesOnly(t *testing.T) {
	r, logs := logRouter(t)

	get(t, r, "/health")
	if n := logs.Len(); n != 0 {
		t.Errorf("logs = %d, want 0 for a successful probe", n)
	}

	get(t, r, "/products/42")
	if n := logs.Len(); n != 1 {
		t.Errorf("logs = %d, want 1 for a real request", n)
	}
}

// A failing probe is the one time a probe is worth reading, so the skip list
// must not swallow it.
func TestLogging_KeepsFailingProbes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	core, logs := observer.New(zapcore.DebugLevel)
	r := gin.New()
	r.Use(httpmw.Logging(zap.New(core)))
	r.GET("/ready", func(c *gin.Context) { c.Status(http.StatusServiceUnavailable) })

	get(t, r, "/ready")

	if logs.Len() != 1 {
		t.Fatalf("logs = %d, want 1 — a failing probe must be kept", logs.Len())
	}
	// 503 is 5xx, so it takes the Error level like any other server failure.
	if lvl := logs.All()[0].Level; lvl != zapcore.ErrorLevel {
		t.Errorf("level = %s, want error for 503", lvl)
	}
}

// One line per request, at the level the status class deserves — never a
// duplicate Info+Error pair.
func TestLogging_LevelFollowsStatusClass(t *testing.T) {
	for _, tc := range []struct {
		path string
		want zapcore.Level
	}{
		{"/products/42", zapcore.InfoLevel},
		{"/missing", zapcore.WarnLevel},
		{"/boom", zapcore.ErrorLevel},
	} {
		t.Run(tc.path, func(t *testing.T) {
			r, logs := logRouter(t)
			get(t, r, tc.path)

			if logs.Len() != 1 {
				t.Fatalf("logs = %d, want exactly 1", logs.Len())
			}
			if got := logs.All()[0].Level; got != tc.want {
				t.Errorf("level = %s, want %s", got, tc.want)
			}
		})
	}
}

// The correlation header is a client contract and always answers, even with no
// span — but that generated value must never be logged as trace_id, or a search
// by it finds nothing in the backend.
func TestLogging_HeaderAlwaysSetButGeneratedIDNeverLogged(t *testing.T) {
	r, logs := logRouter(t)

	rec := get(t, r, "/products/42")

	if got := rec.Header().Get(httpmw.TraceIDHeader); got == "" {
		t.Error("no X-Trace-ID header — the client correlation contract is broken")
	}
	if logs.Len() != 1 {
		t.Fatalf("logs = %d, want 1", logs.Len())
	}
	// No tracer provider is installed here, so there is no span and therefore no
	// trace_id field — the generated header value must not have leaked into it.
	if _, ok := logs.All()[0].ContextMap()["trace_id"]; ok {
		t.Error("trace_id was logged without a span — the generated fallback leaked into telemetry")
	}
}

// The trace-context field is a carrier, not output: the otelzap bridge finds it
// by type-asserting Interface to context.Context, and every zap encoder skips
// SkipType so the raw context never reaches stdout.
//
// obsx.TraceContext is the same five lines and remains the public API. This test
// is what makes the unexported copy here safe: if otelzap ever changes how it
// detects the field, httpmw fails on its own instead of drifting quietly.
func TestTraceContextField_IsCarriedNotEncoded(t *testing.T) {
	gin.SetMode(gin.TestMode)
	core, logs := observer.New(zapcore.DebugLevel)
	r := gin.New()
	r.Use(httpmw.Logging(zap.New(core)))
	r.GET("/products/:id", func(c *gin.Context) { c.Status(http.StatusOK) })

	get(t, r, "/products/42")

	if logs.Len() != 1 {
		t.Fatalf("logs = %d, want 1", logs.Len())
	}
	// The context field must not be encoded into the readable record.
	for k := range logs.All()[0].ContextMap() {
		if k == "otel.trace_context" {
			t.Error("the raw context leaked into encoded output")
		}
	}
	// ...but it must still be present on the entry for the bridge to consume.
	var carried bool
	for _, f := range logs.All()[0].Context {
		if f.Type == zapcore.SkipType && f.Key == "otel.trace_context" {
			if _, ok := f.Interface.(context.Context); ok {
				carried = true
			}
		}
	}
	if !carried {
		t.Error("no SkipType field carrying a context — the otelzap bridge has nothing to read")
	}
}

// User-Agent is attacker-controlled and unbounded: a client can send a hundred
// kilobytes of it on every request. Bounding it caps log volume and cost, and
// settles CodeQL's log-injection finding at the source instead of suppressing it.
func TestLogging_BoundsAttackerControlledValues(t *testing.T) {
	gin.SetMode(gin.TestMode)
	core, logs := observer.New(zapcore.DebugLevel)
	r := gin.New()
	r.Use(httpmw.Logging(zap.New(core)))
	r.GET("/products/:id", func(c *gin.Context) { c.Status(http.StatusOK) })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/products/42", nil)
	req.Header.Set("User-Agent", strings.Repeat("A", 100_000))
	r.ServeHTTP(rec, req)

	ua, _ := logs.All()[0].ContextMap()["user_agent"].(string)
	if len(ua) > 300 {
		t.Errorf("user_agent logged at %d bytes — an unbounded header reached the record", len(ua))
	}
	if !strings.HasSuffix(ua, "(truncated)") {
		t.Error("truncation is not marked — a clipped value reads as the whole thing")
	}
}

// TraceID answers even with no span, because correlate-by-header is a client
// contract. Each fallback rung is pinned so the order cannot silently change.
func TestTraceID_FallbackOrder(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const want = "4bf92f3577b34da6a3ce929d0e0e4736"

	t.Run("traceparent", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		c.Request.Header.Set(httpmw.TraceParentHeader, "00-"+want+"-00f067aa0ba902b7-01")
		if got := httpmw.TraceID(c); got != want {
			t.Errorf("TraceID = %q, want the traceparent trace id", got)
		}
	})

	t.Run("x-trace-id", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		c.Request.Header.Set(httpmw.TraceIDHeader, "from-header")
		if got := httpmw.TraceID(c); got != "from-header" {
			t.Errorf("TraceID = %q, want the X-Trace-ID value", got)
		}
	})

	t.Run("generated when nothing is present", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		if got := httpmw.TraceID(c); len(got) != 32 {
			t.Errorf("TraceID = %q, want a generated 32-hex id", got)
		}
	})

	t.Run("malformed traceparent falls through", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		c.Request.Header.Set(httpmw.TraceParentHeader, "garbage")
		c.Request.Header.Set(httpmw.TraceIDHeader, "from-header")
		if got := httpmw.TraceID(c); got != "from-header" {
			t.Errorf("TraceID = %q, want the fallback when traceparent is malformed", got)
		}
	})
}

// LoggerFrom returns a silent logger when Logging was never mounted. A second,
// uncorrelated logger would hide that mistake; silence surfaces it.
func TestLoggerFrom(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("returns the request logger when mounted", func(t *testing.T) {
		core, logs := observer.New(zapcore.DebugLevel)
		r := gin.New()
		r.Use(httpmw.Logging(zap.New(core)))
		r.GET("/x", func(c *gin.Context) {
			httpmw.LoggerFrom(c).Info("from handler")
			c.Status(http.StatusOK)
		})
		get(t, r, "/x")

		if logs.FilterMessage("from handler").Len() != 1 {
			t.Error("handler log did not go through the request logger")
		}
	})

	t.Run("silent when Logging was not mounted", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		if httpmw.LoggerFrom(c) == nil {
			t.Fatal("LoggerFrom returned nil")
		}
		httpmw.LoggerFrom(c).Info("must not panic")
	})
}

func TestLoggerWithTraceID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("binds the id when present", func(t *testing.T) {
		core, logs := observer.New(zapcore.DebugLevel)
		base := zap.New(core)
		r := gin.New()
		r.Use(httpmw.Logging(zap.NewNop()))
		r.GET("/x", func(c *gin.Context) {
			httpmw.LoggerWithTraceID(c, base).Info("bound")
			c.Status(http.StatusOK)
		})
		get(t, r, "/x")

		if _, ok := logs.All()[0].ContextMap()["trace_id"]; !ok {
			t.Error("trace_id was not bound onto the base logger")
		}
	})

	t.Run("returns the base logger untouched when absent", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		base := zap.NewNop()
		if httpmw.LoggerWithTraceID(c, base) != base {
			t.Error("expected the base logger back when no trace id is set")
		}
	})
}

// A service may add its own routes to the skip list; the defaults still apply.
func TestSkipper_ExtraRoutesAddToDefaults(t *testing.T) {
	gin.SetMode(gin.TestMode)
	core, logs := observer.New(zapcore.DebugLevel)
	r := gin.New()
	r.Use(httpmw.Logging(zap.New(core), "/internal/debug"))
	r.GET("/internal/debug", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.GET("/health", func(c *gin.Context) { c.Status(http.StatusOK) })
	r.GET("/x", func(c *gin.Context) { c.Status(http.StatusOK) })

	get(t, r, "/internal/debug")
	get(t, r, "/health")
	if logs.Len() != 0 {
		t.Errorf("logs = %d, want 0 — extras and defaults must both skip", logs.Len())
	}
	get(t, r, "/x")
	if logs.Len() != 1 {
		t.Errorf("logs = %d, want 1 — a normal route must still log", logs.Len())
	}
}
