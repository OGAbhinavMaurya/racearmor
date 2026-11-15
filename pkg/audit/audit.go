package audit

import (
	"fmt"
	"strings"
	"time"
)

// Status indicates the severity of the timeout hierarchy configuration.
type Status string

const (
	StatusCritical Status = "CRITICAL"
	StatusHigh     Status = "HIGH"
	StatusWarning  Status = "WARNING"
	StatusSafe     Status = "SAFE"
)

// SafetyBuffer is the minimum recommended duration that backend keepalive
// must exceed proxy keepalive to absorb network latency and clock jitter.
const SafetyBuffer = 5 * time.Second

// StandardProxyTiers maps common reverse proxy names to their default keepalive idle timeouts.
var StandardProxyTiers = map[string]time.Duration{
	"alb":           60 * time.Second,
	"aws-alb":       60 * time.Second,
	"cloudflare":    100 * time.Second,
	"nginx":         65 * time.Second,
	"nginx-ingress": 65 * time.Second,
	"gcp":           600 * time.Second,
	"gcp-lb":        600 * time.Second,
	"envoy":         60 * time.Second,
	"traefik":       90 * time.Second,
}

// StandardRuntimeDefaults maps backend runtimes to their default keepalive idle timeouts.
var StandardRuntimeDefaults = map[string]time.Duration{
	"node":     5 * time.Second,
	"nodejs":   5 * time.Second,
	"gunicorn": 2 * time.Second,
	"uvicorn":  5 * time.Second,
	"puma":     20 * time.Second,
	"go":       0 * time.Second, // net/http default is indefinite unless configured
}

// Report captures the evaluation of proxy vs backend keepalive timeouts.
type Report struct {
	ProxyTier                 string        `json:"proxy_tier,omitempty"`
	ProxyTimeout              time.Duration `json:"proxy_timeout"`
	ProxyTimeoutSec           float64       `json:"proxy_timeout_seconds"`
	Runtime                   string        `json:"runtime,omitempty"`
	BackendTimeout            time.Duration `json:"backend_timeout"`
	BackendTimeoutSec         float64       `json:"backend_timeout_seconds"`
	RaceWindow                time.Duration `json:"race_window"`
	RaceWindowSec             float64       `json:"race_window_seconds"`
	Status                    Status        `json:"status"`
	Summary                   string        `json:"summary"`
	RecommendedBackendTimeout time.Duration `json:"recommended_backend_timeout"`
	RecommendedTimeoutSec     float64       `json:"recommended_timeout_seconds"`
	PatchInstructions         string        `json:"patch_instructions,omitempty"`
	CodePatch                 string        `json:"code_patch,omitempty"`
}

// NormalizeTier cleans the tier or runtime string for lookup.
func NormalizeTier(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// LookupProxyTier returns the default idle timeout for a given proxy tier.
func LookupProxyTier(name string) (time.Duration, bool) {
	d, ok := StandardProxyTiers[NormalizeTier(name)]
	return d, ok
}

// LookupRuntimeDefault returns the default idle timeout for a given backend runtime.
func LookupRuntimeDefault(name string) (time.Duration, bool) {
	d, ok := StandardRuntimeDefaults[NormalizeTier(name)]
	return d, ok
}

// Evaluate computes the timeout delta and determines if a 502 race condition window exists.
func Evaluate(proxyTimeout, backendTimeout time.Duration, proxyTier, runtime string) *Report {
	recTimeout := proxyTimeout + SafetyBuffer
	var raceWindow time.Duration
	var status Status
	var summary string

	if backendTimeout < proxyTimeout {
		raceWindow = proxyTimeout - backendTimeout
		if raceWindow >= 10*time.Second {
			status = StatusCritical
			summary = fmt.Sprintf("Critical 502 race window (%v). Proxy idle timeout (%v) exceeds backend (%v). The backend drops idle sockets before the proxy retires them.",
				raceWindow, proxyTimeout, backendTimeout)
		} else {
			status = StatusHigh
			summary = fmt.Sprintf("High 502 race hazard (%v). Proxy idle timeout (%v) exceeds backend (%v). Connections reused near the backend idle threshold will trigger TCP RST.",
				raceWindow, proxyTimeout, backendTimeout)
		}
	} else if backendTimeout < proxyTimeout+SafetyBuffer {
		raceWindow = 0
		status = StatusWarning
		summary = fmt.Sprintf("Narrow safety buffer (%v). Backend (%v) barely exceeds proxy (%v). Network latency spikes could cause sporadic race collisions.",
			backendTimeout-proxyTimeout, backendTimeout, proxyTimeout)
	} else {
		raceWindow = 0
		status = StatusSafe
		summary = fmt.Sprintf("Safe hierarchy. Backend idle timeout (%v) outlives proxy (%v) with adequate safety margin (%v).",
			backendTimeout, proxyTimeout, backendTimeout-proxyTimeout)
	}

	report := &Report{
		ProxyTier:                 proxyTier,
		ProxyTimeout:              proxyTimeout,
		ProxyTimeoutSec:           proxyTimeout.Seconds(),
		Runtime:                   runtime,
		BackendTimeout:            backendTimeout,
		BackendTimeoutSec:         backendTimeout.Seconds(),
		RaceWindow:                raceWindow,
		RaceWindowSec:             raceWindow.Seconds(),
		Status:                    status,
		Summary:                   summary,
		RecommendedBackendTimeout: recTimeout,
		RecommendedTimeoutSec:     recTimeout.Seconds(),
	}

	if runtime != "" {
		instr, patch := GeneratePatch(runtime, recTimeout)
		report.PatchInstructions = instr
		report.CodePatch = patch
	}

	return report
}
