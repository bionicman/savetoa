package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestHelpIsSuccessful(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := Run([]string{"help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("Run(help) code = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "run TARGET") {
		t.Fatalf("help does not describe target execution: %q", stdout.String())
	}
}

func TestVersionIsSuccessful(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := Run([]string{"--version"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("Run(--version) code = %d, want 0", code)
	}
	if !strings.HasPrefix(stdout.String(), "savetoa ") {
		t.Fatalf("unexpected version output: %q", stdout.String())
	}
}

func TestPlannedCommandFailsClosed(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := Run([]string{"run", "production-mariadb"}, &stdout, &stderr)
	if code == 0 {
		t.Fatal("unimplemented backup command returned success")
	}
	if !strings.Contains(stderr.String(), "not implemented") {
		t.Fatalf("unexpected error: %q", stderr.String())
	}
}

func TestUnknownCommandIsRejected(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := Run([]string{"frobnicate"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("Run(unknown) code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("unexpected error: %q", stderr.String())
	}
}
