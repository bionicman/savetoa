package postgresql

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
	standby    bool
	probeErr   error
	captureErr error
	verifyErr  error
	verified   bool
}

func (runner *fakeRunner) Version(context.Context, string) (string, error) { return "18.1", nil }
func (runner *fakeRunner) Probe(context.Context, config.Target) (string, bool, error) {
	return "18.1", runner.standby, runner.probeErr
}
func (runner *fakeRunner) Capture(_ context.Context, target config.Target, directory string) error {
	if target.Driver == "postgresql-base" {
		if err := os.WriteFile(filepath.Join(directory, "base.tar"), []byte("base"), 0o600); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(directory, "pg_wal.tar"), []byte("wal"), 0o600); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(directory, "backup_manifest"), []byte("manifest"), 0o600); err != nil {
			return err
		}
	} else {
		if err := os.WriteFile(filepath.Join(directory, "database.dump"), []byte("dump"), 0o600); err != nil {
			return err
		}
	}
	return runner.captureErr
}
func (runner *fakeRunner) Verify(context.Context, config.Target, string) error {
	runner.verified = true
	return runner.verifyErr
}

func testTarget(t *testing.T, driver string) config.Target {
	t.Helper()
	passfile := filepath.Join(t.TempDir(), "pgpass")
	if err := os.WriteFile(passfile, []byte("localhost:5432:*:backup:secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return config.Target{Driver: driver, Credentials: config.FileReference{File: passfile}, Source: config.MariaDBSource{Host: "localhost", Port: 5432, Username: "backup", Database: "example"}}
}

func TestCaptureModes(t *testing.T) {
	for _, driver := range []string{"postgresql-base", "postgresql-dump"} {
		t.Run(driver, func(t *testing.T) {
			target := testTarget(t, driver)
			if driver == "postgresql-base" {
				target.Source.Database = ""
				required := true
				target.Source.RequireStandby = &required
			}
			runner := &fakeRunner{standby: true}
			capture, err := NewCapturerWithRunner(runner).Capture(context.Background(), target, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if !runner.verified || capture.Report.ToolVersion != "18.1" {
				t.Fatalf("capture report=%+v verified=%v", capture.Report, runner.verified)
			}
			path := capture.Path()
			if err := capture.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("work directory remains: %v", err)
			}
		})
	}
}

func TestBaseRejectsPrimaryAndDoesNotStartCapture(t *testing.T) {
	target := testTarget(t, "postgresql-base")
	target.Source.Database = ""
	required := true
	target.Source.RequireStandby = &required
	runner := &fakeRunner{}
	_, err := NewCapturerWithRunner(runner).Capture(context.Background(), target, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "not a standby") || runner.verified {
		t.Fatalf("unexpected result: %v", err)
	}
}

func TestCaptureFailureCleansPartialOutputAndRedactsNativeError(t *testing.T) {
	target := testTarget(t, "postgresql-dump")
	work := t.TempDir()
	runner := &fakeRunner{captureErr: errors.New("secret-from-native-stderr")}
	_, err := NewCapturerWithRunner(runner).Capture(context.Background(), target, work)
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("native error leaked: %v", err)
	}
	entries, err := os.ReadDir(work)
	if err != nil || len(entries) != 0 {
		t.Fatalf("partial work remains: %v %v", entries, err)
	}
}

func TestPassfileRequiresMode0600AndNoSymlink(t *testing.T) {
	target := testTarget(t, "postgresql-dump")
	if err := os.Chmod(target.Credentials.File, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewCapturerWithRunner(&fakeRunner{}).Check(context.Background(), target); err == nil {
		t.Fatal("loose passfile accepted")
	}
	if err := os.Chmod(target.Credentials.File, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target.Credentials.File, link); err != nil {
		t.Fatal(err)
	}
	target.Credentials.File = link
	if _, err := NewCapturerWithRunner(&fakeRunner{}).Check(context.Background(), target); err == nil {
		t.Fatal("symlink passfile accepted")
	}
}

func TestConnectionEnvironmentContainsPathNotPassword(t *testing.T) {
	target := testTarget(t, "postgresql-dump")
	got := strings.Join(connectionEnvironment(target, target.Source.Database), "\n")
	if strings.Contains(got, "secret") || !strings.Contains(got, "PGPASSFILE="+target.Credentials.File) {
		t.Fatalf("unsafe environment: %q", got)
	}
}

func TestNativeCommandsAreFixedAndSecretFree(t *testing.T) {
	for _, driver := range []string{"postgresql-base", "postgresql-dump"} {
		t.Run(driver, func(t *testing.T) {
			target := testTarget(t, driver)
			if driver == "postgresql-base" {
				target.Source.Database = ""
			}
			capture := captureCommand(context.Background(), target, "/private/work")
			verification := verificationCommand(context.Background(), target, "/private/work", 18)
			joined := strings.Join(append(append([]string{}, capture.Args...), verification.Args...), " ")
			if strings.Contains(joined, "secret") || strings.Contains(joined, target.Credentials.File) {
				t.Fatalf("credentials in command: %q", joined)
			}
			if driver == "postgresql-base" {
				if capture.Path != "/usr/bin/pg_basebackup" || verification.Path != "/usr/lib/postgresql/18/bin/pg_verifybackup" || !slices.Contains(capture.Args, "--wal-method=stream") || !slices.Contains(verification.Args, "--no-parse-wal") {
					t.Fatalf("wrong base commands: %q %q", capture.Args, verification.Args)
				}
			} else if capture.Path != "/usr/bin/pg_dump" || verification.Path != "/usr/bin/pg_restore" || !slices.Contains(capture.Args, "--format=custom") || !slices.Contains(verification.Args, "--file=/dev/null") {
				t.Fatalf("wrong dump commands: %q %q", capture.Args, verification.Args)
			}
		})
	}
}
