package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestMain_Usage(t *testing.T) {
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	printUsage()

	_ = w.Close()
	os.Stdout = oldStdout

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	out := buf.String()

	if !strings.Contains(out, "doctor") || !strings.Contains(out, "shield") || !strings.Contains(out, "audit") {
		t.Errorf("expected usage to contain doctor, shield, audit, got: %s", out)
	}
}

func TestRunAudit_CLI(t *testing.T) {
	// Test valid audit command with --runtime and --proxy
	err := runAudit([]string{"--runtime", "nodejs", "--proxy", "alb"})
	if err != nil {
		t.Errorf("expected runAudit to succeed, got: %v", err)
	}

	// Test missing arguments
	err = runAudit([]string{})
	if err == nil {
		t.Errorf("expected error for empty arguments")
	}
}

func TestRunDoctor_Validation(t *testing.T) {
	// Test doctor without target
	err := runDoctor([]string{})
	if err == nil {
		t.Errorf("expected error when --target is missing")
	}
}
