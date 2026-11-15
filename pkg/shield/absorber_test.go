package shield

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAbsorber_NormalProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			w.Header().Set("X-Echo-Length", fmt.Sprintf("%d", len(body)))
			_, _ = w.Write([]byte("echo: " + string(body)))
			return
		}
		w.Header().Set("X-Custom", "racearmor")
		_, _ = w.Write([]byte("upstream response"))
	}))
	defer upstream.Close()

	abs, err := NewAbsorber(Config{
		ListenAddr:  "127.0.0.1:0",
		UpstreamURL: upstream.URL,
	})
	if err != nil {
		t.Fatalf("failed to create absorber: %v", err)
	}

	go func() {
		_ = abs.Start()
	}()
	defer func() {
		_ = abs.Close()
	}()

	time.Sleep(50 * time.Millisecond)

	proxyURL := "http://" + abs.Addr()

	// Test GET
	resp, err := http.Get(proxyURL + "/test")
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Custom") != "racearmor" {
		t.Errorf("expected X-Custom header")
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "upstream response" {
		t.Errorf("unexpected body: %s", string(body))
	}

	// Test POST
	postResp, err := http.Post(proxyURL+"/submit", "text/plain", strings.NewReader("payload"))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer postResp.Body.Close()

	pbody, _ := io.ReadAll(postResp.Body)
	if string(pbody) != "echo: payload" {
		t.Errorf("unexpected POST body: %s", string(pbody))
	}

	metrics := abs.GetMetrics()
	if metrics.RequestsTotal != 2 {
		t.Errorf("expected 2 total requests, got %d", metrics.RequestsTotal)
	}
	if metrics.RecoveredRaces != 0 {
		t.Errorf("expected 0 recovered races for healthy upstream, got %d", metrics.RecoveredRaces)
	}
}

func TestAbsorber_AbsorbRaceCondition(t *testing.T) {
	// Raw TCP listener that simulates a server dropping the connection on the first attempt,
	// and serving a valid HTTP response on the second attempt.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer l.Close()

	var attempts uint32
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			curr := atomic.AddUint32(&attempts, 1)
			if curr == 1 {
				// Abruptly close connection after read (simulating keepalive race condition EOF/RST)
				buf := make([]byte, 1024)
				_, _ = conn.Read(buf)
				_ = conn.Close()
			} else {
				// Second attempt: valid HTTP response
				buf := make([]byte, 1024)
				_, _ = conn.Read(buf)
				resp := "HTTP/1.1 200 OK\r\nContent-Length: 9\r\nConnection: close\r\n\r\nrecovered"
				_, _ = conn.Write([]byte(resp))
				_ = conn.Close()
			}
		}
	}()

	abs, err := NewAbsorber(Config{
		ListenAddr:  "127.0.0.1:0",
		UpstreamURL: "http://" + l.Addr().String(),
		MaxRetries:  2,
	})
	if err != nil {
		t.Fatalf("failed to create absorber: %v", err)
	}

	go func() {
		_ = abs.Start()
	}()
	defer func() {
		_ = abs.Close()
	}()

	time.Sleep(50 * time.Millisecond)

	req, _ := http.NewRequest(http.MethodGet, "http://"+abs.Addr()+"/flaky", nil)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("expected request to succeed via retry absorber, got: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK after recovery, got %d", resp.StatusCode)
	}
	recoveredHeader := resp.Header.Get("X-RaceArmor-Recovered")
	if recoveredHeader != "1" {
		t.Errorf("expected X-RaceArmor-Recovered: 1, got %q", recoveredHeader)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "recovered" {
		t.Errorf("expected 'recovered', got %q", string(body))
	}

	metrics := abs.GetMetrics()
	if metrics.RecoveredRaces != 1 {
		t.Errorf("expected 1 recovered race, got %d", metrics.RecoveredRaces)
	}
	if metrics.FailedRequests != 0 {
		t.Errorf("expected 0 failed requests, got %d", metrics.FailedRequests)
	}
}

func TestAbsorber_RetriesExhausted(t *testing.T) {
	// A listener that constantly drops connections
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer l.Close()

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	abs, err := NewAbsorber(Config{
		ListenAddr:  "127.0.0.1:0",
		UpstreamURL: "http://" + l.Addr().String(),
		MaxRetries:  1,
	})
	if err != nil {
		t.Fatalf("failed to create absorber: %v", err)
	}

	go func() {
		_ = abs.Start()
	}()
	defer func() {
		_ = abs.Close()
	}()

	time.Sleep(50 * time.Millisecond)

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + abs.Addr() + "/failing")
	if err != nil {
		t.Fatalf("expected HTTP 502 response from proxy, got err: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("expected 502 Bad Gateway, got %d", resp.StatusCode)
	}

	metrics := abs.GetMetrics()
	if metrics.FailedRequests != 1 {
		t.Errorf("expected 1 failed request, got %d", metrics.FailedRequests)
	}
}

func TestAbsorber_Shutdown(t *testing.T) {
	abs, err := NewAbsorber(Config{
		ListenAddr:  "127.0.0.1:0",
		UpstreamURL: "http://127.0.0.1:9999",
	})
	if err != nil {
		t.Fatalf("failed to create absorber: %v", err)
	}

	go func() {
		_ = abs.Start()
	}()
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	if err := abs.Shutdown(ctx); err != nil {
		t.Errorf("failed to shutdown cleanly: %v", err)
	}
}
