package obsx

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// emitDenied writes one span carrying every denied key, one allowed key, and a
// span event with a denied key, through whatever provider tp is.
func emitDenied(tp trace.TracerProvider) {
	_, span := tp.Tracer("privacy-test").Start(context.Background(), "GET /x",
		trace.WithAttributes(
			attribute.String("client.address", "10.0.0.7"),
			attribute.String("network.peer.address", "10.0.0.7"),
			attribute.Int("network.peer.port", 51234),
			attribute.String("user_agent.original", "curl/8.0"),
			attribute.String("db.connection_string", "redis://cache:6379"),
			attribute.String("http.route", "/x"),
		))
	span.AddEvent("hop", trace.WithAttributes(
		attribute.String("user_agent.original", "curl/8.0"),
		attribute.String("kept", "yes"),
	))
	span.End()
}

func assertStripped(t *testing.T, spans tracetest.SpanStubs) {
	t.Helper()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	s := spans[0]
	for _, a := range s.Attributes {
		if _, denied := deniedSpanKeys[a.Key]; denied {
			t.Errorf("denied span attribute %q was exported", a.Key)
		}
	}
	if !hasKey(s.Attributes, "http.route") {
		t.Error("allowed attribute http.route was dropped")
	}
	if len(s.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(s.Events))
	}
	if hasKey(s.Events[0].Attributes, "user_agent.original") {
		t.Error("denied event attribute user_agent.original was exported")
	}
	if !hasKey(s.Events[0].Attributes, "kept") {
		t.Error("allowed event attribute was dropped")
	}
}

func hasKey(kv []attribute.KeyValue, k attribute.Key) bool {
	for _, a := range kv {
		if a.Key == k {
			return true
		}
	}
	return false
}

func TestSpanDenyList_StockProvider(t *testing.T) {
	ctx := context.Background()
	exp := tracetest.NewInMemoryExporter()
	obs, err := SetupObservability(ctx,
		Config{ServiceName: "t", TracesEnabled: true, SampleRate: 1},
		withSpanExporter(exp))
	if err != nil {
		t.Fatal(err)
	}
	emitDenied(obs.TracerProvider())
	if err := obs.ForceFlush(ctx); err != nil {
		t.Fatal(err)
	}
	assertStripped(t, exp.GetSpans())
	_ = obs.Shutdown(ctx)
}

// The Temporal services build their provider through the factory; the filter
// lives under the batcher obsx assembles, so it must hold there too.
func TestSpanDenyList_FactoryProvider(t *testing.T) {
	ctx := context.Background()
	exp := tracetest.NewInMemoryExporter()
	obs, err := SetupObservability(ctx,
		Config{ServiceName: "t", TracesEnabled: true, SampleRate: 1},
		withSpanExporter(exp),
		WithTracerProviderFactory(func(c TracerProviderConfig) ShutdownTracerProvider {
			return &fakeFactoryProvider{sdktrace.NewTracerProvider(c.SDKOptions()...)}
		}))
	if err != nil {
		t.Fatal(err)
	}
	emitDenied(obs.TracerProvider())
	if err := obs.ForceFlush(ctx); err != nil {
		t.Fatal(err)
	}
	assertStripped(t, exp.GetSpans())
	_ = obs.Shutdown(ctx)
}

// A span with nothing denied is passed through as the same value: the common
// path must not allocate a copy per span.
func TestSpanDenyList_CleanSpanUntouched(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	_, span := tp.Tracer("t").Start(context.Background(), "clean",
		trace.WithAttributes(attribute.String("http.route", "/y")))
	span.End()
	ro := exp.GetSpans().Snapshots()[0]
	got, changed := stripSpan(ro)
	if _, wrapped := got.(strippedSpan); changed || wrapped {
		t.Error("a span with no denied key must be returned unchanged")
	}
	if _, changed := withoutDenied(nil); changed {
		t.Error("nil attributes reported as changed")
	}
}
