package obsx

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// deniedSpanKeys is the part of the ADR-071 deny list that instrumentation
// libraries write on spans by themselves: otelgin and otelhttp stamp every HTTP
// server span with the client and peer address and the full User-Agent. The
// logging facade strips these from records; spans never went through it, so
// the exporter drops them here, before anything leaves the process.
var deniedSpanKeys = map[attribute.Key]struct{}{
	"client.address":       {},
	"network.peer.address": {},
	"network.peer.port":    {},
	"user_agent.original":  {},
}

// privacyExporter removes deniedSpanKeys from span and span-event attributes
// before handing the batch to the real exporter. It sits under the batcher, so
// it covers the stock provider and a WithTracerProviderFactory one alike.
type privacyExporter struct {
	sdktrace.SpanExporter
}

func (e privacyExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	out, copied := spans, false
	for i, s := range spans {
		stripped, changed := stripSpan(s)
		if !changed {
			continue
		}
		if !copied { // the batcher owns spans; copy before the first write
			out, copied = append([]sdktrace.ReadOnlySpan(nil), spans...), true
		}
		out[i] = stripped
	}
	return e.SpanExporter.ExportSpans(ctx, out)
}

// strippedSpan overrides the attribute-bearing accessors of a span; everything
// else (ids, status, timing, resource) comes from the original.
type strippedSpan struct {
	sdktrace.ReadOnlySpan
	attrs  []attribute.KeyValue
	events []sdktrace.Event
}

func (s strippedSpan) Attributes() []attribute.KeyValue { return s.attrs }
func (s strippedSpan) Events() []sdktrace.Event         { return s.events }

func stripSpan(s sdktrace.ReadOnlySpan) (sdktrace.ReadOnlySpan, bool) {
	attrs, attrsChanged := withoutDenied(s.Attributes())
	events := s.Events()
	eventsChanged := false
	for i, ev := range events {
		kept, changed := withoutDenied(ev.Attributes)
		if !changed {
			continue
		}
		if !eventsChanged {
			events = append([]sdktrace.Event(nil), events...)
			eventsChanged = true
		}
		events[i].Attributes = kept
	}
	if !attrsChanged && !eventsChanged {
		return s, false
	}
	return strippedSpan{ReadOnlySpan: s, attrs: attrs, events: events}, true
}

// withoutDenied returns kv without denied keys, and whether anything was
// removed. The common case — nothing denied — allocates nothing.
func withoutDenied(kv []attribute.KeyValue) ([]attribute.KeyValue, bool) {
	for i, a := range kv {
		if _, denied := deniedSpanKeys[a.Key]; !denied {
			continue
		}
		kept := make([]attribute.KeyValue, 0, len(kv)-1)
		kept = append(kept, kv[:i]...)
		for _, rest := range kv[i+1:] {
			if _, d := deniedSpanKeys[rest.Key]; !d {
				kept = append(kept, rest)
			}
		}
		return kept, true
	}
	return kv, false
}
