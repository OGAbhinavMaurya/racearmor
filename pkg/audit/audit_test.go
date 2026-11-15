package audit

import (
	"strings"
	"testing"
	"time"
)

func TestEvaluate_Critical(t *testing.T) {
	// ALB (60s) vs Node.js default (5s) -> 55s race window
	rep := Evaluate(60*time.Second, 5*time.Second, "alb", "node")
	if rep.Status != StatusCritical {
		t.Errorf("expected StatusCritical, got %s", rep.Status)
	}
	if rep.RaceWindow != 55*time.Second {
		t.Errorf("expected 55s race window, got %v", rep.RaceWindow)
	}
	if rep.RecommendedBackendTimeout != 65*time.Second {
		t.Errorf("expected 65s recommended timeout, got %v", rep.RecommendedBackendTimeout)
	}
	if !strings.Contains(rep.CodePatch, "server.keepAliveTimeout = 65000") {
		t.Errorf("expected node code patch to contain 65000, got: %s", rep.CodePatch)
	}
}

func TestEvaluate_Warning(t *testing.T) {
	// Proxy 60s, Backend 62s (less than 60s + 5s buffer)
	rep := Evaluate(60*time.Second, 62*time.Second, "alb", "puma")
	if rep.Status != StatusWarning {
		t.Errorf("expected StatusWarning, got %s", rep.Status)
	}
	if rep.RaceWindow != 0 {
		t.Errorf("expected 0 race window, got %v", rep.RaceWindow)
	}
}

func TestEvaluate_Safe(t *testing.T) {
	// Proxy 60s, Backend 70s (exceeds 65s)
	rep := Evaluate(60*time.Second, 70*time.Second, "alb", "go")
	if rep.Status != StatusSafe {
		t.Errorf("expected StatusSafe, got %s", rep.Status)
	}
	if rep.RaceWindow != 0 {
		t.Errorf("expected 0 race window, got %v", rep.RaceWindow)
	}
}

func TestLookup(t *testing.T) {
	albTimeout, ok := LookupProxyTier("alb")
	if !ok || albTimeout != 60*time.Second {
		t.Errorf("expected alb 60s, got %v, %v", albTimeout, ok)
	}

	cfTimeout, ok := LookupProxyTier("CLOUDFLARE")
	if !ok || cfTimeout != 100*time.Second {
		t.Errorf("expected cloudflare 100s, got %v, %v", cfTimeout, ok)
	}

	_, ok = LookupProxyTier("unknown-proxy")
	if ok {
		t.Errorf("expected unknown proxy to return false")
	}

	nodeTimeout, ok := LookupRuntimeDefault("nodejs")
	if !ok || nodeTimeout != 5*time.Second {
		t.Errorf("expected nodejs 5s, got %v, %v", nodeTimeout, ok)
	}

	gunTimeout, ok := LookupRuntimeDefault("gunicorn")
	if !ok || gunTimeout != 2*time.Second {
		t.Errorf("expected gunicorn 2s, got %v, %v", gunTimeout, ok)
	}
}

func TestGeneratePatch(t *testing.T) {
	runtimes := []string{"node", "gunicorn", "uvicorn", "puma", "go", "other"}
	for _, rt := range runtimes {
		instr, patch := GeneratePatch(rt, 65*time.Second)
		if instr == "" || patch == "" {
			t.Errorf("empty instructions or patch for %s", rt)
		}
	}
}
