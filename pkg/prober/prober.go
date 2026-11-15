package prober

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Config configures a keepalive probe.
type Config struct {
	TargetURL string
	MaxWait   time.Duration
	Headers   map[string]string
}

// Result contains the measurement output of a keepalive probe.
type Result struct {
	TargetURL        string        `json:"target_url"`
	StatusCode       int           `json:"status_code"`
	BackendTimeout   time.Duration `json:"backend_timeout"`
	BackendTimeoutSec float64      `json:"backend_timeout_seconds"`
	ClosedByServer   bool          `json:"closed_by_server"`
	TimedOut         bool          `json:"timed_out"`
	ConnectionHeader string        `json:"connection_header"`
	ErrorMessage     string        `json:"error,omitempty"`
}

// Prober performs active TCP keepalive disconnect timing measurements.
type Prober struct {
	dialer net.Dialer
}

// New creates a new Prober instance.
func New() *Prober {
	return &Prober{
		dialer: net.Dialer{
			Timeout: 10 * time.Second,
		},
	}
}

// Probe connects to the target URL over HTTP/1.1, executes an initial request
// with Connection: keep-alive, and precisely measures the idle duration until the
// backend closes the TCP socket (EOF or RST).
func (p *Prober) Probe(cfg Config) (*Result, error) {
	if cfg.TargetURL == "" {
		return nil, errors.New("target URL must not be empty")
	}
	if cfg.MaxWait <= 0 {
		cfg.MaxWait = 70 * time.Second
	}

	rawURL := cfg.TargetURL
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		rawURL = "http://" + rawURL
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid target URL: %w", err)
	}

	host := parsed.Hostname()
	port := parsed.Port()
	isTLS := strings.EqualFold(parsed.Scheme, "https")

	if port == "" {
		if isTLS {
			port = "443"
		} else {
			port = "80"
		}
	}
	address := net.JoinHostPort(host, port)

	var conn net.Conn
	if isTLS {
		tlsConfig := &tls.Config{
			ServerName: host,
			MinVersion: tls.VersionTLS12,
		}
		conn, err = tls.DialWithDialer(&p.dialer, "tcp", address, tlsConfig)
	} else {
		conn, err = p.dialer.Dial("tcp", address)
	}
	if err != nil {
		return &Result{
			TargetURL:    cfg.TargetURL,
			ErrorMessage: fmt.Sprintf("dial failed: %v", err),
		}, err
	}
	defer conn.Close()

	path := parsed.RequestURI()
	if path == "" {
		path = "/"
	}

	req, err := http.NewRequest(http.MethodGet, path, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to construct request: %w", err)
	}
	req.Host = parsed.Host
	req.Header.Set("User-Agent", "racearmor-prober/1.0")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Connection", "keep-alive")

	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}

	if err := req.Write(conn); err != nil {
		return &Result{
			TargetURL:    cfg.TargetURL,
			ErrorMessage: fmt.Sprintf("failed to write HTTP request: %v", err),
		}, err
	}

	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		return &Result{
			TargetURL:    cfg.TargetURL,
			ErrorMessage: fmt.Sprintf("failed to read HTTP response: %v", err),
		}, err
	}

	// Drain and close body
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	connHeader := resp.Header.Get("Connection")
	if connHeader == "" && resp.Close {
		connHeader = "close"
	}
	statusCode := resp.StatusCode

	if resp.Close || strings.EqualFold(connHeader, "close") {
		return &Result{
			TargetURL:         cfg.TargetURL,
			StatusCode:        statusCode,
			BackendTimeout:    0,
			BackendTimeoutSec: 0,
			ClosedByServer:    true,
			TimedOut:          false,
			ConnectionHeader:  connHeader,
		}, nil
	}

	// Measure idle duration until server tears down connection
	startIdle := time.Now()
	_ = conn.SetReadDeadline(startIdle.Add(cfg.MaxWait))

	buf := make([]byte, 1024)
	for {
		_, readErr := reader.Read(buf)
		if readErr != nil {
			elapsed := time.Since(startIdle)
			if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
				return &Result{
					TargetURL:         cfg.TargetURL,
					StatusCode:        statusCode,
					BackendTimeout:    elapsed,
					BackendTimeoutSec: elapsed.Seconds(),
					ClosedByServer:    true,
					TimedOut:          false,
					ConnectionHeader:  connHeader,
				}, nil
			}

			var netErr net.Error
			if (errors.As(readErr, &netErr) && netErr.Timeout()) || errors.Is(readErr, os.ErrDeadlineExceeded) {
				return &Result{
					TargetURL:         cfg.TargetURL,
					StatusCode:        statusCode,
					BackendTimeout:    cfg.MaxWait,
					BackendTimeoutSec: cfg.MaxWait.Seconds(),
					ClosedByServer:    false,
					TimedOut:          true,
					ConnectionHeader:  connHeader,
				}, nil
			}

			// Connection reset by peer or closed
			return &Result{
				TargetURL:         cfg.TargetURL,
				StatusCode:        statusCode,
				BackendTimeout:    elapsed,
				BackendTimeoutSec: elapsed.Seconds(),
				ClosedByServer:    true,
				TimedOut:          false,
				ConnectionHeader:  connHeader,
			}, nil
		}
	}
}
