package httpmw

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// Transport must inject the caller's trace context and record a client span;
// Handler must continue that trace on the receiving side. One round trip
// through both proves the whole hop stays in one trace.
func TestTransportAndHandlerShareOneTrace(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	var gotTraceparent string
	srv := httptest.NewServer(Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTraceparent = r.Header.Get("traceparent")
		w.WriteHeader(http.StatusNoContent)
	}), "provider.webhook"))
	t.Cleanup(srv.Close)

	ctx, parent := tp.Tracer("test").Start(context.Background(), "charge")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: Transport(nil)}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	parent.End()

	if gotTraceparent == "" {
		t.Fatal("Transport injected no traceparent")
	}
	want := parent.SpanContext().TraceID()
	var kinds []trace.SpanKind
	for _, s := range rec.Ended() {
		if s.SpanContext().TraceID() != want {
			t.Fatalf("span %q left the trace: %s != %s", s.Name(), s.SpanContext().TraceID(), want)
		}
		kinds = append(kinds, s.SpanKind())
	}
	var client1, server1 bool
	for _, k := range kinds {
		client1 = client1 || k == trace.SpanKindClient
		server1 = server1 || k == trace.SpanKindServer
	}
	if !client1 || !server1 {
		t.Fatalf("want one client and one server span in the trace, got kinds %v", kinds)
	}
}
