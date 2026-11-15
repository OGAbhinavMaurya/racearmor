package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/OGAbhinavMaurya/racearmor/pkg/audit"
	"github.com/OGAbhinavMaurya/racearmor/pkg/prober"
	"github.com/OGAbhinavMaurya/racearmor/pkg/shield"
)

const version = "1.0.0"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	command := os.Args[1]
	args := os.Args[2:]

	var err error
	switch command {
	case "doctor":
		err = runDoctor(args)
	case "audit":
		err = runAudit(args)
	case "shield":
		err = runShield(args)
	case "version", "-v", "--version":
		fmt.Printf("racearmor version %s\n", version)
		return
	case "help", "-h", "--help":
		printUsage()
		return
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", command)
		printUsage()
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func printUsage() {
	usage := `racearmor - HTTP keep-alive race condition sentry and 502 eliminator

Usage:
  racearmor <command> [options]

Commands:
  doctor     Probe live backend, measure keepalive disconnect, and evaluate 502 risk
  audit      Audit proxy and runtime timeout configuration with remediation code
  shield     Run zero-code reverse proxy absorber to transparently eliminate 502s
  version    Show racearmor version
  help       Show this help message

Doctor Options:
  --target         Target backend URL to probe (e.g. http://127.0.0.1:3000)
  --proxy          Upstream proxy tier (alb, cloudflare, nginx, gcp, envoy, traefik) [default: alb]
  --proxy-timeout  Explicit proxy idle timeout duration (e.g. 60s)
  --max-wait       Maximum duration to wait for server socket closure [default: 70s]
  --runtime        Backend runtime for remediation generation (node, gunicorn, uvicorn, puma, go)
  --json           Output results in JSON format

Audit Options:
  --runtime        Backend runtime to evaluate (node, gunicorn, uvicorn, puma, go)
  --backend-timeout Custom backend keepalive duration (e.g. 5s; defaults to runtime standard)
  --proxy          Upstream proxy tier (alb, cloudflare, nginx, gcp, envoy, traefik) [default: alb]
  --proxy-timeout  Explicit proxy idle timeout duration (e.g. 60s)
  --json           Output results in JSON format

Shield Options:
  --listen         Local bind address [default: :8080]
  --upstream       Upstream backend URL to protect [required, e.g. http://127.0.0.1:3000]
  --retries        Maximum retries for premature socket disconnects [default: 2]
  --max-body       Maximum request body buffering size in bytes [default: 10485760 (10MB)]
  --timeout        Proxy read/write timeout [default: 65s]
  --json           Output operational metrics in JSON upon exit

Examples:
  racearmor doctor --target http://127.0.0.1:3000 --proxy alb
  racearmor audit --runtime nodejs --proxy cloudflare
  racearmor shield --listen :8080 --upstream http://127.0.0.1:3000 --retries 2
`
	fmt.Print(usage)
}

func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	target := fs.String("target", "", "Target backend URL")
	proxyTier := fs.String("proxy", "alb", "Upstream proxy tier")
	proxyTimeoutStr := fs.String("proxy-timeout", "", "Explicit proxy idle timeout")
	maxWait := fs.Duration("max-wait", 70*time.Second, "Maximum probe duration")
	runtime := fs.String("runtime", "", "Backend runtime")
	jsonOutput := fs.Bool("json", false, "Output in JSON format")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *target == "" {
		return fmt.Errorf("--target is required (e.g. --target http://127.0.0.1:3000)")
	}

	proxyTimeout := 60 * time.Second
	if *proxyTimeoutStr != "" {
		d, err := time.ParseDuration(*proxyTimeoutStr)
		if err != nil {
			return fmt.Errorf("invalid --proxy-timeout: %w", err)
		}
		proxyTimeout = d
	} else if *proxyTier != "" {
		if d, ok := audit.LookupProxyTier(*proxyTier); ok {
			proxyTimeout = d
		}
	}

	p := prober.New()
	probeRes, err := p.Probe(prober.Config{
		TargetURL: *target,
		MaxWait:   *maxWait,
	})
	if err != nil {
		return fmt.Errorf("probe failed: %w", err)
	}

	report := audit.Evaluate(proxyTimeout, probeRes.BackendTimeout, *proxyTier, *runtime)

	if *jsonOutput {
		out := map[string]interface{}{
			"probe":  probeRes,
			"report": report,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	fmt.Println("================================================================================")
	fmt.Println("                            RACEARMOR DOCTOR REPORT                             ")
	fmt.Println("================================================================================")
	fmt.Printf(" Target URL:             %s\n", probeRes.TargetURL)
	fmt.Printf(" HTTP Status Code:       %d\n", probeRes.StatusCode)
	fmt.Printf(" Connection Header:      %s\n", probeRes.ConnectionHeader)
	if probeRes.ClosedByServer {
		fmt.Printf(" Measured Backend Idle:  %v\n", probeRes.BackendTimeout)
	} else if probeRes.TimedOut {
		fmt.Printf(" Measured Backend Idle:  >= %v (timed out without disconnect)\n", probeRes.BackendTimeout)
	}
	fmt.Printf(" Upstream Proxy Tier:    %s (%v)\n", *proxyTier, proxyTimeout)
	fmt.Println("--------------------------------------------------------------------------------")
	fmt.Printf(" Status:                 [%s]\n", report.Status)
	fmt.Printf(" Race Window:            %v\n", report.RaceWindow)
	fmt.Printf(" Recommended Backend:    >= %v\n", report.RecommendedBackendTimeout)
	fmt.Printf(" Summary:                %s\n", report.Summary)
	if report.CodePatch != "" {
		fmt.Println("--------------------------------------------------------------------------------")
		fmt.Printf(" Remediation:\n%s\n", report.CodePatch)
	}
	fmt.Println("================================================================================")

	return nil
}

func runAudit(args []string) error {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	runtime := fs.String("runtime", "", "Backend runtime")
	backendTimeoutStr := fs.String("backend-timeout", "", "Backend timeout")
	proxyTier := fs.String("proxy", "alb", "Upstream proxy tier")
	proxyTimeoutStr := fs.String("proxy-timeout", "", "Explicit proxy idle timeout")
	jsonOutput := fs.Bool("json", false, "Output in JSON format")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *runtime == "" && *backendTimeoutStr == "" {
		return fmt.Errorf("either --runtime or --backend-timeout must be provided")
	}

	var backendTimeout time.Duration
	if *backendTimeoutStr != "" {
		d, err := time.ParseDuration(*backendTimeoutStr)
		if err != nil {
			return fmt.Errorf("invalid --backend-timeout: %w", err)
		}
		backendTimeout = d
	} else if *runtime != "" {
		if d, ok := audit.LookupRuntimeDefault(*runtime); ok {
			backendTimeout = d
		} else {
			backendTimeout = 5 * time.Second
		}
	}

	proxyTimeout := 60 * time.Second
	if *proxyTimeoutStr != "" {
		d, err := time.ParseDuration(*proxyTimeoutStr)
		if err != nil {
			return fmt.Errorf("invalid --proxy-timeout: %w", err)
		}
		proxyTimeout = d
	} else if *proxyTier != "" {
		if d, ok := audit.LookupProxyTier(*proxyTier); ok {
			proxyTimeout = d
		}
	}

	report := audit.Evaluate(proxyTimeout, backendTimeout, *proxyTier, *runtime)

	if *jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}

	fmt.Println("================================================================================")
	fmt.Println("                             RACEARMOR AUDIT REPORT                             ")
	fmt.Println("================================================================================")
	fmt.Printf(" Proxy Tier:             %s (Idle Timeout: %v)\n", *proxyTier, proxyTimeout)
	fmt.Printf(" Backend Runtime:        %s (Keepalive Idle: %v)\n", *runtime, backendTimeout)
	fmt.Println("--------------------------------------------------------------------------------")
	fmt.Printf(" Status:                 [%s]\n", report.Status)
	fmt.Printf(" 502 Race Window:        %v\n", report.RaceWindow)
	fmt.Printf(" Recommended Backend:    >= %v\n", report.RecommendedBackendTimeout)
	fmt.Printf(" Summary:                %s\n", report.Summary)
	if report.CodePatch != "" {
		fmt.Println("--------------------------------------------------------------------------------")
		fmt.Printf(" Recommended Fix (%s):\n%s\n", *runtime, report.CodePatch)
	}
	fmt.Println("================================================================================")

	return nil
}

func runShield(args []string) error {
	fs := flag.NewFlagSet("shield", flag.ContinueOnError)
	listen := fs.String("listen", ":8080", "Local listen address")
	upstream := fs.String("upstream", "", "Upstream backend URL")
	retries := fs.Int("retries", 2, "Maximum retry attempts")
	maxBody := fs.Int64("max-body", 10*1024*1024, "Maximum body size in bytes")
	timeout := fs.Duration("timeout", 65*time.Second, "Read/write timeout")
	jsonOutput := fs.Bool("json", false, "Output metrics as JSON upon exit")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *upstream == "" {
		return fmt.Errorf("--upstream is required (e.g. --upstream http://127.0.0.1:3000)")
	}

	abs, err := shield.NewAbsorber(shield.Config{
		ListenAddr:   *listen,
		UpstreamURL:  *upstream,
		MaxRetries:   *retries,
		MaxBodyBytes: *maxBody,
		ReadTimeout:  *timeout,
		WriteTimeout: *timeout,
		IdleTimeout:  *timeout + 10*time.Second,
	})
	if err != nil {
		return fmt.Errorf("failed to initialize shield absorber: %w", err)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	serverErr := make(chan error, 1)
	go func() {
		fmt.Printf("racearmor shield listening on %s -> protecting %s (max retries: %d)\n", *listen, *upstream, *retries)
		serverErr <- abs.Start()
	}()

	select {
	case sig := <-sigChan:
		fmt.Fprintf(os.Stderr, "\nReceived signal %v, shutting down racearmor shield...\n", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = abs.Shutdown(ctx)
	case err := <-serverErr:
		if err != nil && !strings.Contains(err.Error(), "Server closed") {
			return fmt.Errorf("server error: %w", err)
		}
	}

	metrics := abs.GetMetrics()
	if *jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(metrics)
	}

	fmt.Println("--------------------------------------------------------------------------------")
	fmt.Printf("Shield Summary:\n  Total Requests:    %d\n  Recovered Races:   %d\n  Failed Requests:   %d\n",
		metrics.RequestsTotal, metrics.RecoveredRaces, metrics.FailedRequests)
	fmt.Println("--------------------------------------------------------------------------------")

	return nil
}
