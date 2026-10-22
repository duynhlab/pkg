package dbx

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// The acquire check is on unless a caller turns it off. Pure option logic.
func TestWithAcquireCheck(t *testing.T) {
	var c config
	if c.noAcquireCheck {
		t.Fatal("acquire check must default to on")
	}
	WithAcquireCheck(false)(&c)
	if !c.noAcquireCheck {
		t.Fatal("WithAcquireCheck(false) did not turn the check off")
	}
	WithAcquireCheck(true)(&c)
	if c.noAcquireCheck {
		t.Fatal("WithAcquireCheck(true) did not turn the check back on")
	}
}

// A server error is named by its SQLSTATE, even when wrapped; anything else
// (dial, TLS, timeout) is "connect".
func TestConnectErrorType(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"wrong password", &pgconn.PgError{Code: "28P01"}, "28P01"},
		{"wrapped shutdown", fmt.Errorf("connect: %w", &pgconn.PgError{Code: "57P01"}), "57P01"},
		{"dial failure", errors.New("dial tcp: connection refused"), "connect"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := connectErrorType(tt.err); got != tt.want {
				t.Errorf("connectErrorType = %q, want %q", got, tt.want)
			}
		})
	}
}

// Only a failed connect is counted, labelled by error.type; the query hooks
// leave the context untouched.
func TestConnectFailureTracer(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	failures, err := mp.Meter(meterName).Int64Counter(metricConnectFailures)
	if err != nil {
		t.Fatal(err)
	}
	tr := connectFailureTracer{failures: failures}
	ctx := context.Background()

	if got := tr.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{}); got != ctx {
		t.Error("TraceQueryStart changed the context")
	}
	tr.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})
	if got := tr.TraceConnectStart(ctx, pgx.TraceConnectStartData{}); got != ctx {
		t.Error("TraceConnectStart changed the context")
	}
	tr.TraceConnectEnd(ctx, pgx.TraceConnectEndData{})
	tr.TraceConnectEnd(ctx, pgx.TraceConnectEndData{Err: &pgconn.PgError{Code: "28P01"}})
	tr.TraceConnectEnd(ctx, pgx.TraceConnectEndData{Err: &pgconn.PgError{Code: "28P01"}})
	tr.TraceConnectEnd(ctx, pgx.TraceConnectEndData{Err: errors.New("timeout")})

	got := counterByAttr(t, reader, metricConnectFailures, "error.type")
	want := map[string]int64{"28P01": 2, "connect": 1}
	if len(got) != len(want) || got["28P01"] != 2 || got["connect"] != 1 {
		t.Errorf("failures = %v, want %v", got, want)
	}
}

// counterByAttr collects reader and returns the named Int64 counter's points
// keyed by the value of attribute key ("" when the point lacks it).
func counterByAttr(t *testing.T, reader *sdkmetric.ManualReader, name, key string) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is %T, want Sum[int64]", name, m.Data)
			}
			for _, dp := range sum.DataPoints {
				v, _ := dp.Attributes.Value(attribute.Key(key))
				out[v.AsString()] += dp.Value
			}
		}
	}
	return out
}

// socketIdle tells an idle open socket from one with bytes waiting or closed
// by the peer, through a TLS wrapper too, and never consumes the byte.
func TestSocketIdle(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}

	wait := func(want bool) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for {
			idle, known := socketIdle(tls.Client(client, &tls.Config{}))
			if !known {
				t.Fatal("socketIdle could not inspect a TCP socket")
			}
			if idle == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("idle = %v, want %v", idle, want)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	wait(true)
	if _, err := server.Write([]byte{'E'}); err != nil {
		t.Fatal(err)
	}
	wait(false)
	buf := make([]byte, 1)
	if _, err := client.Read(buf); err != nil || buf[0] != 'E' {
		t.Fatalf("peek consumed the byte: read %q, %v", buf, err)
	}
	wait(true)
	server.Close()
	wait(false)

	if _, known := socketIdle(&net.TCPConn{}); known {
		t.Error("a TCPConn with no descriptor must be reported unknown")
	}
}

// pgconn's background reader can sit in a blocking Read on an idle socket.
// socketIdle must still answer at once instead of waiting for that Read's lock
// (the Read-based peek in dbx v0.39.0–v0.40.0 hung here forever).
func TestSocketIdleWhileAnotherReadBlocks(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		_, _ = client.Read(make([]byte, 1)) // blocks: the server sends nothing yet
	}()
	time.Sleep(50 * time.Millisecond) // let the Read take the descriptor's read lock

	type result struct{ idle, known bool }
	got := make(chan result, 1)
	go func() {
		idle, known := socketIdle(client)
		got <- result{idle, known}
	}()
	select {
	case r := <-got:
		if !r.known || !r.idle {
			t.Errorf("socketIdle = idle %v, known %v; want an idle, known socket", r.idle, r.known)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("socketIdle blocked behind a concurrent Read")
	}

	if _, err := server.Write([]byte{'Z'}); err != nil { // release the reader
		t.Fatal(err)
	}
	<-readDone
}
