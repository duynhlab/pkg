package grpcx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// capture is a slog.Handler that keeps every record with the context it was
// written with, so a test asserts both the attributes and the correlation.
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

func (h *capture) byMessage(msg string) []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []slog.Record
	for _, r := range h.recs {
		if r.Message == msg {
			out = append(out, r)
		}
	}
	return out
}

func (h *capture) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.recs)
}

func attrsOf(r slog.Record) map[string]string {
	m := map[string]string{}
	r.Attrs(func(a slog.Attr) bool { m[a.Key] = a.Value.String(); return true })
	return m
}

func newLogger() (*slog.Logger, *capture) {
	h := &capture{}
	return slog.New(h), h
}

// The access record carries exactly the canonical attributes of the pinned
// semantic conventions, with the values the server span carries.
func TestAccessLogUnary_CanonicalAttributes(t *testing.T) {
	logger, h := newLogger()
	info := &grpc.UnaryServerInfo{FullMethod: "/product.v1.ProductService/ReserveStock"}
	if _, err := accessLogUnary(logger)(context.Background(), nil, info,
		func(context.Context, any) (any, error) { return "ok", nil }); err != nil {
		t.Fatal(err)
	}
	recs := h.byMessage("gRPC request")
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	r := recs[0]
	if r.Level != slog.LevelInfo {
		t.Errorf("level = %v, want Info", r.Level)
	}
	a := attrsOf(r)
	want := map[string]string{
		"rpc.system.name":          "grpc",
		"rpc.method":               "product.v1.ProductService/ReserveStock",
		"rpc.response.status_code": "OK",
	}
	for k, v := range want {
		if a[k] != v {
			t.Errorf("%s = %q, want %q", k, a[k], v)
		}
	}
	if len(a) != len(want) {
		t.Errorf("want exactly %d attributes on success, got %v", len(want), a)
	}
	for _, forbidden := range []string{"method", "code", "duration", "peer", "trace_id", "rpc.service", "network.peer.address"} {
		if _, ok := a[forbidden]; ok {
			t.Errorf("forbidden attribute %q present", forbidden)
		}
	}
}

// The record is written with the call's context, so a context-aware handler
// stamps the SAME trace id as the active span — the log↔trace join.
func TestAccessLogUnary_CorrelatesThroughTheContext(t *testing.T) {
	logger, h := newLogger()
	tid, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	sid, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	ctx := trace.ContextWithSpanContext(context.Background(),
		trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled}))
	info := &grpc.UnaryServerInfo{FullMethod: "/x.v1.S/M"}
	_, _ = accessLogUnary(logger)(ctx, nil, info, func(context.Context, any) (any, error) { return nil, nil })
	h.mu.Lock()
	got := trace.SpanContextFromContext(h.ctxs[0])
	h.mu.Unlock()
	if got.TraceID() != tid || got.SpanID() != sid {
		t.Errorf("record context must carry the active span: %v", got)
	}
}

// Severity is who should look; error.type is whether the server failed, as the
// span decides it. The two axes differ on purpose.
func TestAccessLogUnary_LevelAndErrorTypeFollowTheCode(t *testing.T) {
	cases := []struct {
		code      codes.Code
		level     slog.Level
		canonical string
		errType   bool
	}{
		{codes.OK, slog.LevelInfo, "OK", false},
		{codes.NotFound, slog.LevelInfo, "NOT_FOUND", false},
		{codes.Canceled, slog.LevelInfo, "CANCELLED", false},
		{codes.AlreadyExists, slog.LevelInfo, "ALREADY_EXISTS", false},
		{codes.InvalidArgument, slog.LevelInfo, "INVALID_ARGUMENT", false},
		{codes.Unauthenticated, slog.LevelInfo, "UNAUTHENTICATED", false},
		{codes.DeadlineExceeded, slog.LevelWarn, "DEADLINE_EXCEEDED", true},
		{codes.PermissionDenied, slog.LevelWarn, "PERMISSION_DENIED", false},
		{codes.ResourceExhausted, slog.LevelWarn, "RESOURCE_EXHAUSTED", false},
		{codes.FailedPrecondition, slog.LevelWarn, "FAILED_PRECONDITION", false},
		{codes.Aborted, slog.LevelWarn, "ABORTED", false},
		{codes.OutOfRange, slog.LevelWarn, "OUT_OF_RANGE", false},
		{codes.Unavailable, slog.LevelWarn, "UNAVAILABLE", true},
		{codes.Unknown, slog.LevelError, "UNKNOWN", true},
		{codes.Unimplemented, slog.LevelError, "UNIMPLEMENTED", true},
		{codes.Internal, slog.LevelError, "INTERNAL", true},
		{codes.DataLoss, slog.LevelError, "DATA_LOSS", true},
	}
	for _, tc := range cases {
		logger, h := newLogger()
		info := &grpc.UnaryServerInfo{FullMethod: "/x.v1.S/M"}
		_, _ = accessLogUnary(logger)(context.Background(), nil, info,
			func(context.Context, any) (any, error) {
				if tc.code == codes.OK {
					return nil, nil
				}
				return nil, status.Error(tc.code, "x")
			})
		r := h.byMessage("gRPC request")[0]
		a := attrsOf(r)
		if r.Level != tc.level {
			t.Errorf("%v: level = %v, want %v", tc.code, r.Level, tc.level)
		}
		if a["rpc.response.status_code"] != tc.canonical {
			t.Errorf("%v: status = %q, want %q", tc.code, a["rpc.response.status_code"], tc.canonical)
		}
		if _, has := a["error.type"]; has != tc.errType {
			t.Errorf("%v: error.type present = %v, want %v", tc.code, has, tc.errType)
		}
		if tc.errType && a["error.type"] != tc.canonical {
			t.Errorf("%v: error.type = %q, want %q", tc.code, a["error.type"], tc.canonical)
		}
	}
}

// A code this build does not know is a fault, not a quiet success.
func TestCodeLevel_UnknownCodeIsError(t *testing.T) {
	if got := codeLevel(codes.Code(99)); got != slog.LevelError {
		t.Errorf("unknown code level = %v, want Error", got)
	}
	if got := canonicalCode(codes.Code(99)); got != "CODE(99)" {
		t.Errorf("unknown code canonical = %q, want CODE(99)", got)
	}
}

// A handler that returns a bare context error reaches the client as CANCELLED
// or DEADLINE_EXCEEDED; the record must say the same, not UNKNOWN.
func TestAccessLogUnary_ContextErrorsMapToTheirCodes(t *testing.T) {
	for err, want := range map[error]string{
		context.Canceled:                            "CANCELLED",
		context.DeadlineExceeded:                    "DEADLINE_EXCEEDED",
		fmt.Errorf("wrapped: %w", context.Canceled): "CANCELLED",
		errors.New("plain"):                         "UNKNOWN",
	} {
		logger, h := newLogger()
		info := &grpc.UnaryServerInfo{FullMethod: "/x.v1.S/M"}
		_, _ = accessLogUnary(logger)(context.Background(), nil, info,
			func(context.Context, any) (any, error) { return nil, err })
		if got := attrsOf(h.byMessage("gRPC request")[0])["rpc.response.status_code"]; got != want {
			t.Errorf("%v: status = %q, want %q", err, got, want)
		}
	}
}

func TestAccessLogUnary_SkipsInfraMethods(t *testing.T) {
	logger, h := newLogger()
	info := &grpc.UnaryServerInfo{FullMethod: "/grpc.health.v1.Health/Check"}
	_, _ = accessLogUnary(logger)(context.Background(), nil, info,
		func(context.Context, any) (any, error) { return nil, nil })
	if n := h.count(); n != 0 {
		t.Errorf("infra RPC produced %d records, want 0", n)
	}
}

func TestAccessLogUnary_NilLoggerIsNoop(t *testing.T) {
	called := false
	_, err := accessLogUnary(nil)(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/x.Y/Z"},
		func(context.Context, any) (any, error) { called = true; return nil, nil })
	if err != nil || !called {
		t.Errorf("nil logger must still call the handler: called=%v err=%v", called, err)
	}
}

func TestIsInfraMethod(t *testing.T) {
	cases := map[string]bool{
		"/grpc.health.v1.Health/Check":                              true,
		"/grpc.reflection.v1.ServerReflection/ServerReflectionInfo": true,
		"/product.v1.ProductService/ReserveStock":                   false,
		"/review.v1.ReviewService/GetProductReviews":                false,
	}
	for m, want := range cases {
		if got := isInfraMethod(m); got != want {
			t.Errorf("isInfraMethod(%q) = %v, want %v", m, got, want)
		}
	}
}

// fakeServerStream satisfies grpc.ServerStream just enough for the access-log
// interceptor, which only reads Context().
type fakeServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (f fakeServerStream) Context() context.Context { return f.ctx }

func TestAccessLogStream_LogsOncePerStream(t *testing.T) {
	logger, h := newLogger()
	info := &grpc.StreamServerInfo{FullMethod: "/product.v1.ProductService/WatchStock"}
	err := accessLogStream(logger)(nil, fakeServerStream{ctx: context.Background()}, info,
		func(any, grpc.ServerStream) error { return status.Error(codes.Internal, "boom") })
	if status.Code(err) != codes.Internal {
		t.Fatalf("handler error not forwarded: %v", err)
	}
	recs := h.byMessage("gRPC request")
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	a := attrsOf(recs[0])
	if a["rpc.method"] != "product.v1.ProductService/WatchStock" || a["rpc.response.status_code"] != "INTERNAL" || a["error.type"] != "INTERNAL" {
		t.Errorf("stream record: %v", a)
	}
}

func TestAccessLogStream_SkipsInfraAndNilLogger(t *testing.T) {
	logger, h := newLogger()
	info := &grpc.StreamServerInfo{FullMethod: "/grpc.health.v1.Health/Watch"}
	if err := accessLogStream(logger)(nil, fakeServerStream{ctx: context.Background()}, info,
		func(any, grpc.ServerStream) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if n := h.count(); n != 0 {
		t.Errorf("infra stream produced %d records, want 0", n)
	}
	called := false
	if err := accessLogStream(nil)(nil, fakeServerStream{ctx: context.Background()},
		&grpc.StreamServerInfo{FullMethod: "/x.Y/Z"},
		func(any, grpc.ServerStream) error { called = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Error("nil-logger stream interceptor must still call the handler")
	}
}

// panicDesc registers one unary method "/grpcx.test.Panic/Boom" whose handler
// panics — it invokes the server's chained interceptor so recovery + access
// log run exactly as in production.
var panicDesc = grpc.ServiceDesc{
	ServiceName: "grpcx.test.Panic",
	HandlerType: (*any)(nil),
	Methods: []grpc.MethodDesc{{
		MethodName: "Boom",
		Handler: func(_ any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
			if err := dec(&emptypb.Empty{}); err != nil {
				return nil, err
			}
			impl := func(context.Context, any) (any, error) { panic("boom: token=hunter2") }
			info := &grpc.UnaryServerInfo{FullMethod: "/grpcx.test.Panic/Boom"}
			if interceptor == nil {
				return impl(ctx, nil)
			}
			return interceptor(ctx, &emptypb.Empty{}, info, impl)
		},
	}},
	Metadata: "test",
}

func serve(t *testing.T, logger *slog.Logger) *grpc.ClientConn {
	t.Helper()
	srv, _ := NewServer(logger)
	srv.RegisterService(&panicDesc, struct{}{})
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := Dial(lis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func invokePanic(t *testing.T, conn *grpc.ClientConn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := conn.Invoke(ctx, "/grpcx.test.Panic/Boom", &emptypb.Empty{}, &emptypb.Empty{})
	if status.Code(err) != codes.Internal {
		t.Fatalf("recovered panic should surface as codes.Internal, got %v", status.Code(err))
	}
}

// The regression guard for the interceptor chain order, over the wire through
// NewServer: a panicking handler surfaces as ONE access record at Error with
// status INTERNAL, and the panic itself is one structured record — never free
// text on stderr, which would bypass the facade's redaction.
func TestNewServer_RecoveredPanicIsStructuredAndLoggedAsInternal(t *testing.T) {
	logger, h := newLogger()
	invokePanic(t, serve(t, logger))

	access := h.byMessage("gRPC request")
	if len(access) != 1 {
		t.Fatalf("want 1 access record for the panicking RPC, got %d", len(access))
	}
	if access[0].Level != slog.LevelError || attrsOf(access[0])["rpc.response.status_code"] != "INTERNAL" {
		t.Errorf("access record: %v %v", access[0].Level, attrsOf(access[0]))
	}
	panics := h.byMessage("gRPC handler panicked")
	if len(panics) != 1 {
		t.Fatalf("want 1 panic record, got %d", len(panics))
	}
	a := attrsOf(panics[0])
	if a["rpc.system.name"] != "grpc" || a["rpc.method"] != "grpcx.test.Panic/Boom" ||
		a["error.type"] != "panic" || a["exception.message"] != "!PANIC (string): boom: token=hunter2" {
		t.Errorf("panic record: %v", a)
	}
	if st := a["exception.stacktrace"]; st == "" || len(st) > maxPanicStack+len(truncatedMarker) {
		t.Errorf("stacktrace must be present and bounded to %d bytes, got %d", maxPanicStack, len(st))
	}
}

// With the access log disabled (nil logger) a panic is still reported — through
// slog.Default — and still answered with INTERNAL rather than crashing.
func TestNewServer_NilLoggerStillReportsPanics(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	def, h := newLogger()
	slog.SetDefault(def)

	invokePanic(t, serve(t, nil))
	if n := len(h.byMessage("gRPC handler panicked")); n != 1 {
		t.Errorf("panic must reach slog.Default when no logger is given, got %d records", n)
	}
	if n := len(h.byMessage("gRPC request")); n != 0 {
		t.Errorf("nil logger disables the access log, got %d access records", n)
	}
}

func TestRPCMethod(t *testing.T) {
	long := "/" + strings.Repeat("a", maxRPCMethod+10)
	for in, want := range map[string]string{
		"/a.B/C": "a.B/C",
		"a.B/C":  "a.B/C",
		"":       "",
		long:     strings.Repeat("a", maxRPCMethod) + truncatedMarker,
	} {
		if got := rpcMethod(in); got != want {
			t.Errorf("rpcMethod(%q) = %q, want %q", in, got, want)
		}
	}
}

type secretPanic struct{ password string }

type panickyErr struct{}

func (panickyErr) Error() string { panic("Error() itself panics") }

// The panic value is never rendered: only a string or error payload contributes
// text, bounded on a rune boundary; an Error() that panics cannot escape.
func TestPanicMessage(t *testing.T) {
	cases := map[string]struct {
		in   any
		want string
	}{
		"string":        {"boom", "!PANIC (string): boom"},
		"error":         {errors.New("bad"), "!PANIC (*errors.errorString): bad"},
		"struct hidden": {secretPanic{password: "hunter2"}, "!PANIC (grpcx.secretPanic)"},
		"int hidden":    {42, "!PANIC (int)"},
		"panicky Error": {panickyErr{}, "!PANIC (grpcx.panickyErr): error whose Error() panics"},
	}
	for name, tc := range cases {
		if got := panicMessage(tc.in); got != tc.want {
			t.Errorf("%s: panicMessage = %q, want %q", name, got, tc.want)
		}
	}
	long := panicMessage(strings.Repeat("é", 300))
	if !strings.HasSuffix(long, truncatedMarker) || !utf8.ValidString(long) || len(long) > maxPanicMessage+len(truncatedMarker) {
		t.Errorf("long message not bounded on a rune boundary: %q", long)
	}
}
