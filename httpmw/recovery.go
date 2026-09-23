package httpmw

import (
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/gin-gonic/gin"
)

// Recovery returns the platform's panic recovery for gin: a panicking handler
// answers 500 instead of taking the process down, and the panic is reported as
// ONE structured record through logger — never gin's default text output, which
// prints the raw path and client address to stdout past the logging facade.
//
// Mount it after Logging, so it runs inside it: the recovered request reaches
// Logging's summary as a 500 at Error with error.type=panic, and the
// one-summary-per-call rule holds. A handler that had already written its
// status keeps it on the wire (it cannot be changed after the fact); the
// summary then carries that status, still at Error with error.type=panic. Build the engine with gin.New(), not gin.Default(), which installs
// gin's own recovery and logger outside this chain:
//
//	r := gin.New()
//	r.Use(httpmw.Tracing(name), httpmw.Logging(log), httpmw.Recovery(log))
//
// A nil logger still reports the panic, through slog.Default resolved at the
// moment of the panic, so a later slog.SetDefault is honoured. net/http's
// ErrAbortHandler is re-raised untouched: it is the sanctioned way to abort a
// response, not a fault.
func Recovery(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			r := recover()
			if r == nil {
				return
			}
			if err, ok := r.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(r)
			}
			l := logger
			if l == nil {
				l = slog.Default()
			}
			attrs := []slog.Attr{
				slog.String("http.request.method", normalizeMethod(c.Request.Method)),
			}
			if route := c.FullPath(); route != "" {
				attrs = append(attrs, slog.String("http.route", route))
			}
			attrs = append(attrs,
				slog.String("error.type", "panic"),
				slog.String("exception.message", panicMessage(r)),
				slog.String("exception.stacktrace", bound(string(debug.Stack()), maxPanicStack)),
			)
			l.LogAttrs(c.Request.Context(), slog.LevelError, "HTTP handler panicked", attrs...)
			c.Set(ctxKeyPanicked, true)
			c.AbortWithStatus(http.StatusInternalServerError)
		}()
		c.Next()
	}
}
