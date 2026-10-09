package mariadbdump

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bionicman/savetoa/internal/config"
)

type fakeRunner struct {
	version    string
	server     string
	payload    string
	captureErr error
	called     int
}

func (runner *fakeRunner) Version(context.Context) (string, error) { return runner.version, nil }
func (runner *fakeRunner) Probe(context.Context, config.Target) (string, error) {
	return runner.server, nil
}
func (runner *fakeRunner) Capture(_ context.Context, _ config.Target, path string) error {
	runner.called++
	if runner.captureErr != nil {
		return runner.captureErr
	}
	return os.WriteFile(path, []byte(runner.payload), 0o600)
}

func validTarget(t *testing.T) config.Target {
	t.Helper()
	credential := filepath.Join(t.TempDir(), "mariadb.cnf")
	if err := os.WriteFile(credential, []byte("[client]\nuser=backup\npassword=unused\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return config.Target{
		Driver:      "mariadb-dump",
		Credentials: config.FileReference{File: credential},
		Source:      config.MariaDBSource{Socket: "/run/mysqld/mysqld.sock", Database: "mailserver"},
	}
}

func TestCaptureCreatesValidatedDump(t *testing.T) {
	target := validTarget(t)
	work := t.TempDir()
	runner := &fakeRunner{version: "11.8.6", server: "11.8.6", payload: "CREATE TABLE smoke (id INT);\n-- Dump completed\n"}
	capture, err := NewCapturerWithRunner(runner).Capture(context.Background(), target, work)
	if err != nil {
		t.Fatal(err)
	}
	path := capture.Path()
	if runner.called != 1 {
		t.Fatalf("capture calls=%d", runner.called)
	}
	info, err := os.Stat(filepath.Join(path, dumpName))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("dump info=%v err=%v", info, err)
	}
	if err := capture.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("work directory remains: %v", err)
	}
}

func TestCaptureRejectsTruncatedDumpAndCleansWork(t *testing.T) {
	target := validTarget(t)
	work := t.TempDir()
	runner := &fakeRunner{version: "11.8.6", server: "11.8.6", payload: "CREATE TABLE truncated (id INT);\n"}
	if _, err := NewCapturerWithRunner(runner).Capture(context.Background(), target, work); err == nil || !strings.Contains(err.Error(), "completion marker") {
		t.Fatalf("Capture(truncated) error=%v", err)
	}
	entries, err := os.ReadDir(work)
	if err != nil || len(entries) != 0 {
		t.Fatalf("work entries=%v err=%v", entries, err)
	}
}

func TestCheckRejectsVersionMismatchAndUnsafeCredential(t *testing.T) {
	target := validTarget(t)
	if _, err := NewCapturerWithRunner(&fakeRunner{version: "11.8.5", server: "11.8.6"}).Check(context.Background(), target); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("version mismatch error=%v", err)
	}
	if err := os.Chmod(target.Credentials.File, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := NewCapturerWithRunner(&fakeRunner{version: "11.8.6", server: "11.8.6"}).Check(context.Background(), target); err == nil || !strings.Contains(err.Error(), "mode-0600") {
		t.Fatalf("unsafe credential error=%v", err)
	}
}

func TestArgumentsAreFixedAndCredentialIsFirst(t *testing.T) {
	target := config.Target{
		Credentials: config.FileReference{File: "/etc/savetoa/credentials.d/mail.cnf"},
		Source:      config.MariaDBSource{Socket: "/run/mysqld/mysqld.sock", Database: "mailserver"},
	}
	args := dumpArgs(target)
	if args[0] != "--defaults-extra-file=/etc/savetoa/credentials.d/mail.cnf" {
		t.Fatalf("first argument=%q", args[0])
	}
	for _, required := range []string{"--single-transaction", "--quick", "--routines", "--events", "--triggers", "--hex-blob", "--skip-dump-date", "--databases", "mailserver"} {
		if !slices.Contains(args, required) {
			t.Fatalf("missing fixed argument %q in %#v", required, args)
		}
	}
	if strings.Join(args, " ") != strings.Join(dumpArgs(target), " ") {
		t.Fatal("dump arguments are not deterministic")
	}
}

func TestVersionParsing(t *testing.T) {
	for input, want := range map[string]string{
		"/usr/bin/mariadb-dump from 11.8.6-MariaDB, client 10.19": "11.8.6",
		"11.8.6-MariaDB-5ubuntu0.1 from Ubuntu":                   "11.8.6",
	} {
		got, err := extractVersion(input)
		if err != nil || got != want {
			t.Fatalf("extractVersion(%q)=%q,%v want=%q", input, got, err, want)
		}
	}
}
