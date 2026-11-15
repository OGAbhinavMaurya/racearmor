package prober

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProber_MeasureDisconnect(t *testing.T) {
	// Create listener for raw HTTP server with explicit IdleTimeout
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer l.Close()

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("ok"))
		}),
		IdleTimeout: 100 * time.Millisecond,
	}

	go func() {
		_ = srv.Serve(l)
	}()
	defer func() {
		_ = srv.Close()
	}()

	p := New()
	res, err := p.Probe(Config{
		TargetURL: fmt.Sprintf("http://%s/", l.Addr().String()),
		MaxWait:   1 * time.Second,
	})
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}

	if res.StatusCode != 200 {
		t.Errorf("expected 200, got %d", res.StatusCode)
	}
	if !res.ClosedByServer {
		t.Errorf("expected ClosedByServer to be true")
	}
	if res.TimedOut {
		t.Errorf("expected TimedOut to be false")
	}
	// Measured timeout should be approximately 100ms (50ms - 500ms tolerance)
	if res.BackendTimeout < 50*time.Millisecond || res.BackendTimeout > 700*time.Millisecond {
		t.Errorf("unexpected measured timeout: %v", res.BackendTimeout)
	}
}

func TestProber_ConnectionClose(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close")
		_, _ = w.Write([]byte("closing"))
	}))
	defer ts.Close()

	p := New()
	res, err := p.Probe(Config{
		TargetURL: ts.URL,
		MaxWait:   500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}

	if res.BackendTimeout != 0 {
		t.Errorf("expected 0 backend timeout for Connection: close, got %v", res.BackendTimeout)
	}
	if !res.ClosedByServer {
		t.Errorf("expected ClosedByServer = true")
	}
}

func TestProber_Timeout(t *testing.T) {
	// Server with long idle timeout
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer l.Close()

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("ok"))
		}),
		IdleTimeout: 10 * time.Second,
	}

	go func() {
		_ = srv.Serve(l)
	}()
	defer func() {
		_ = srv.Close()
	}()

	p := New()
	res, err := p.Probe(Config{
		TargetURL: fmt.Sprintf("http://%s/", l.Addr().String()),
		MaxWait:   80 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("probe failed: %v", err)
	}

	if !res.TimedOut {
		t.Errorf("expected TimedOut to be true")
	}
	if res.ClosedByServer {
		t.Errorf("expected ClosedByServer to be false")
	}
}

func TestProber_InvalidURL(t *testing.T) {
	p := New()
	_, err := p.Probe(Config{TargetURL: ""})
	if err == nil {
		t.Errorf("expected error on empty target URL")
	}
}
