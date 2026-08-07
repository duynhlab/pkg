package dbx

import (
	"context"
	"strings"
	"testing"
	"time"

	nopmetric "go.opentelemetry.io/otel/metric/noop"
	noptrace "go.opentelemetry.io/otel/trace/noop"
)

func TestWithTracerProvider_SetsProvider(t *testing.T) {
	tp := noptrace.NewTracerProvider()
	var cfg config
	WithTracerProvider(tp)(&cfg)
	if cfg.tracerProvider != tp {
		t.Errorf("tracerProvider = %v, want the injected provider", cfg.tracerProvider)
	}
}

func TestWithMeterProvider_SetsProvider(t *testing.T) {
	mp := nopmetric.NewMeterProvider()
	var cfg config
	WithMeterProvider(mp)(&cfg)
	if cfg.meterProvider != mp {
		t.Errorf("meterProvider = %v, want the injected provider", cfg.meterProvider)
	}
}

// A syntactically valid DSN pointing at a closed port drives NewPool through
// config assembly, tracer wiring and pool creation, and must fail on the ping
// — closing the pool rather than returning a half-alive one.
func TestNewPool_PingFailureIsWrappedAndPoolClosed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := NewPool(ctx, "postgres://u:p@127.0.0.1:1/db?sslmode=disable&connect_timeout=1")
	if err == nil {
		pool.Close()
		t.Fatal("NewPool succeeded against a closed port")
	}
	if pool != nil {
		t.Errorf("pool = %v, want nil on error", pool)
	}
	if !strings.Contains(err.Error(), "dbx: ping") {
		t.Errorf("err = %v, want the dbx: ping wrap", err)
	}
}
