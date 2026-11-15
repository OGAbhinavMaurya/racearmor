package shield

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Config configures the shield absorber reverse proxy.
type Config struct {
	ListenAddr   string
	UpstreamURL  string
	MaxRetries   int
	MaxBodyBytes int64
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	IdleTimeout  time.Duration
}

// Metrics tracks request throughput and recovered race collisions.
type Metrics struct {
	RequestsTotal  uint64 `json:"requests_total"`
	RecoveredRaces uint64 `json:"recovered_races"`
	FailedRequests uint64 `json:"failed_requests"`
}

// Absorber is a transparent loopback reverse proxy sidecar that intercepts
// TCP RST / EOF disconnects caused by keepalive race conditions and replays
// the in-flight request over a fresh connection before a 502 Bad Gateway can occur.
type Absorber struct {
	cfg        Config
	targetURL  *url.URL
	server     *http.Server
	transport  *http.Transport
	metrics    Metrics
	mu         sync.Mutex
	running    bool
	listener   net.Listener
}

// NewAbsorber creates a new reverse proxy absorber.
func NewAbsorber(cfg Config) (*Absorber, error) {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8080"
	}
	if cfg.UpstreamURL == "" {
		return nil, errors.New("upstream URL must not be empty")
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 2
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 10 * 1024 * 1024 // 10MB
	}
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = 65 * time.Second
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = 65 * time.Second
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 75 * time.Second
	}

	rawUpstream := cfg.UpstreamURL
	if !strings.HasPrefix(rawUpstream, "http://") && !strings.HasPrefix(rawUpstream, "https://") {
		rawUpstream = "http://" + rawUpstream
	}
	target, err := url.Parse(rawUpstream)
	if err != nil {
		return nil, fmt.Errorf("invalid upstream URL: %w", err)
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          100,
		IdleConnTimeout:       cfg.IdleTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: false,
		},
	}

	abs := &Absorber{
		cfg:       cfg,
		targetURL: target,
		transport: transport,
	}

	abs.server = &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      abs,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		IdleTimeout:  cfg.IdleTimeout,
	}

	return abs, nil
}

// ServeHTTP handles incoming HTTP requests, buffers the payload, forwards to upstream,
// and catches premature socket closures to transparently replay the request.
func (a *Absorber) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	atomic.AddUint64(&a.metrics.RequestsTotal, 1)

	var bodyBytes []byte
	if r.Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(io.LimitReader(r.Body, a.cfg.MaxBodyBytes+1))
		_ = r.Body.Close()
		if err != nil {
			http.Error(w, "Failed to read request body", http.StatusBadRequest)
			return
		}
		if int64(len(bodyBytes)) > a.cfg.MaxBodyBytes {
			http.Error(w, "Request body exceeds maximum allowed size", http.StatusRequestEntityTooLarge)
			return
		}
	}

	var lastErr error
	for attempt := 0; attempt <= a.cfg.MaxRetries; attempt++ {
		// Construct forward request
		outURL := *a.targetURL
		outURL.Path = singleJoiningSlash(a.targetURL.Path, r.URL.Path)
		outURL.RawQuery = r.URL.RawQuery

		var bodyReader io.Reader
		if len(bodyBytes) > 0 {
			bodyReader = bytes.NewReader(bodyBytes)
		}

		outReq, err := http.NewRequestWithContext(r.Context(), r.Method, outURL.String(), bodyReader)
		if err != nil {
			http.Error(w, "Internal proxy request creation failed", http.StatusInternalServerError)
			return
		}

		// Copy headers
		for k, vv := range r.Header {
			for _, v := range vv {
				outReq.Header.Add(k, v)
			}
		}

		// Forwarding headers
		if clientIP, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			prior := outReq.Header.Get("X-Forwarded-For")
			if prior != "" {
				clientIP = prior + ", " + clientIP
			}
			outReq.Header.Set("X-Forwarded-For", clientIP)
		}
		outReq.Header.Set("X-Forwarded-Proto", "http")
		if r.TLS != nil {
			outReq.Header.Set("X-Forwarded-Proto", "https")
		}

		resp, err := a.transport.RoundTrip(outReq)
		if err != nil {
			lastErr = err
			if a.isRetryableDisconnect(err) && attempt < a.cfg.MaxRetries {
				atomic.AddUint64(&a.metrics.RecoveredRaces, 1)
				// Discard idle connection pool to guarantee fresh socket
				a.transport.CloseIdleConnections()
				time.Sleep(10 * time.Millisecond)
				continue
			}
			// Not retryable or exhausted
			break
		}

		// Success response received
		defer resp.Body.Close()

		// If this was recovered from an earlier keepalive collision, tag header
		if attempt > 0 {
			w.Header().Set("X-RaceArmor-Recovered", fmt.Sprintf("%d", attempt))
		}

		for k, vv := range resp.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}

		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}

	// If all retries failed
	atomic.AddUint64(&a.metrics.FailedRequests, 1)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	_, _ = fmt.Fprintf(w, `{"error":"Bad Gateway","details":%q,"racearmor_mitigated":false}`+"\n", lastErr.Error())
}

// isRetryableDisconnect determines if an error is an abrupt connection drop
// typical of a keepalive idle timeout collision.
func (a *Absorber) isRetryableDisconnect(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return true
	}

	errStr := strings.ToLower(err.Error())
	if strings.Contains(errStr, "eof") ||
		strings.Contains(errStr, "connection reset") ||
		strings.Contains(errStr, "broken pipe") ||
		strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "server closed idle connection") {
		return true
	}

	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if opErr.Op == "read" || opErr.Op == "write" || opErr.Op == "dial" {
			return true
		}
	}

	return false
}

// GetMetrics returns snapshot of current operational metrics.
func (a *Absorber) GetMetrics() Metrics {
	return Metrics{
		RequestsTotal:  atomic.LoadUint64(&a.metrics.RequestsTotal),
		RecoveredRaces: atomic.LoadUint64(&a.metrics.RecoveredRaces),
		FailedRequests: atomic.LoadUint64(&a.metrics.FailedRequests),
	}
}

// Start begins listening and serving traffic.
func (a *Absorber) Start() error {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return errors.New("absorber is already running")
	}

	ln, err := net.Listen("tcp", a.cfg.ListenAddr)
	if err != nil {
		a.mu.Unlock()
		return fmt.Errorf("failed to listen on %s: %w", a.cfg.ListenAddr, err)
	}
	a.listener = ln
	a.running = true
	a.mu.Unlock()

	return a.server.Serve(ln)
}

// Addr returns the bound address of the listener if active.
func (a *Absorber) Addr() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.listener != nil {
		return a.listener.Addr().String()
	}
	return a.cfg.ListenAddr
}

// Shutdown gracefully stops the absorber.
func (a *Absorber) Shutdown(ctx context.Context) error {
	a.mu.Lock()
	a.running = false
	a.mu.Unlock()

	a.transport.CloseIdleConnections()
	return a.server.Shutdown(ctx)
}

// Close immediately closes the listener and transport.
func (a *Absorber) Close() error {
	a.mu.Lock()
	a.running = false
	a.mu.Unlock()

	a.transport.CloseIdleConnections()
	return a.server.Close()
}

func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	}
	return a + b
}
