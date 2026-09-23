package httpmw_test

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/duynhlab/pkg/httpmw"
)

type secretPanic struct {
	User     string
	password string
}

func recoveryRouter(h *capture, handler gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(httpmw.Logging(slog.New(h)), httpmw.Recovery(slog.New(h)))
	r.GET("/items/:id", handler)
	return r
}

// A panicking handler answers 500, writes ONE panic record, and Logging's
// summary still follows as an ordinary 500.
func TestRecovery_ReportsOnceAndAnswers500(t *testing.T) {
	h := &capture{}
	r := recoveryRouter(h, func(*gin.Context) { panic("boom") })
	if rec := get(t, r, "/items/42"); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if h.len() != 2 {
		t.Fatalf("records = %d, want the panic record and the access summary", h.len())
	}
	h.mu.Lock()
	panicRec := h.recs[0]
	h.mu.Unlock()
	if panicRec.Level != slog.LevelError || panicRec.Message != "HTTP handler panicked" {
		t.Errorf("panic record = %v %q", panicRec.Level, panicRec.Message)
	}
	a := map[string]slog.Value{}
	panicRec.Attrs(func(x slog.Attr) bool { a[x.Key] = x.Value; return true })
	for k, want := range map[string]string{
		"http.request.method": "GET",
		"http.route":          "/items/:id",
		"error.type":          "panic",
		"exception.message":   "!PANIC (string): boom",
	} {
		if got := a[k].String(); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if !strings.Contains(a["exception.stacktrace"].String(), "goroutine") {
		t.Error("the panic record must carry the stack")
	}
	_, summary, _ := h.last(t)
	if summary["http.response.status_code"].Int64() != 500 {
		t.Error("the access summary must record the recovered request as a 500")
	}
}

// The panic value is never rendered: a struct's fields — unexported ones
// included — must not reach the record.
func TestRecovery_DoesNotRenderThePanicValue(t *testing.T) {
	h := &capture{}
	r := recoveryRouter(h, func(*gin.Context) { panic(secretPanic{User: "alice", password: "hunter2"}) })
	get(t, r, "/items/1")
	h.mu.Lock()
	rec := h.recs[0]
	h.mu.Unlock()
	rec.Attrs(func(x slog.Attr) bool {
		if s := x.Value.String(); strings.Contains(s, "hunter2") || (x.Key != "exception.stacktrace" && strings.Contains(s, "alice")) {
			t.Errorf("%s leaks the panic value: %q", x.Key, s)
		}
		return true
	})
}

// An error payload contributes its text; a long one is bounded on a rune
// boundary and marked.
func TestRecovery_ErrorPayloadIsBounded(t *testing.T) {
	h := &capture{}
	long := strings.Repeat("é", 300)
	r := recoveryRouter(h, func(*gin.Context) { panic(errors.New(long)) })
	get(t, r, "/items/1")
	h.mu.Lock()
	rec := h.recs[0]
	h.mu.Unlock()
	var msg string
	rec.Attrs(func(x slog.Attr) bool {
		if x.Key == "exception.message" {
			msg = x.Value.String()
		}
		return true
	})
	if !strings.HasPrefix(msg, "!PANIC (*errors.errorString): é") || !strings.HasSuffix(msg, "…(truncated)") {
		t.Errorf("exception.message = %q", msg)
	}
	if !utf8Valid(msg) || len(msg) > 256+len("…(truncated)") {
		t.Errorf("exception.message not bounded on a rune boundary: %d bytes", len(msg))
	}
}

// http.ErrAbortHandler is net/http's sanctioned abort, not a fault: re-raised
// untouched and never reported.
func TestRecovery_ReraisesAbortHandler(t *testing.T) {
	h := &capture{}
	r := recoveryRouter(h, func(*gin.Context) { panic(http.ErrAbortHandler) })
	defer func() {
		if got := recover(); got != http.ErrAbortHandler {
			t.Errorf("recovered %v, want http.ErrAbortHandler re-raised", got)
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, rec := range h.recs {
			if rec.Message == "HTTP handler panicked" {
				t.Error("an abort must not be reported as a panic")
			}
		}
	}()
	get(t, r, "/items/1")
}

// A nil logger still reports, through the default logger at panic time.
func TestRecovery_NilLoggerUsesDefaultAtPanicTime(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(httpmw.Recovery(nil))
	r.GET("/x", func(*gin.Context) { panic("boom") })

	h := &capture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if rec := get(t, r, "/x"); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	if h.len() != 1 {
		t.Errorf("records = %d, want the panic reported through slog.Default", h.len())
	}
}

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "�") == s }
