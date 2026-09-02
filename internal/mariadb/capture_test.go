package mariadb

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type captureRunner struct {
	calls           [][]string
	failBackup      bool
	omitMetadata    bool
	leaveUnprepared bool
	credentialPath  string
	cancelBackup    context.CancelFunc
	resumeCtxErr    error
}

func (runner *captureRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	runner.calls = append(runner.calls, append([]string{name}, args...))
	if name == clientPath {
		if slices.Contains(args, "--vertical") {
			return []byte(`*************************** 1. row ***************************
Master_Host: fd00::10
Master_User: backup_replication
Master_Port: 3306
Slave_IO_Running: Yes
Slave_SQL_Running: Yes
Seconds_Behind_Master: 0
Last_IO_Errno: 0
Last_SQL_Errno: 0
Using_Gtid: Slave_Pos
Gtid_IO_Pos: 1-2-9
`), nil
		}
		if hasArgumentPrefix(args, "--execute=SELECT ") {
			return []byte("12.3.3-MariaDB\tON\t1\t1002\t/run/mysqld/mysqld.sock\n"), nil
		}
		if slices.Contains(args, "--execute=START REPLICA SQL_THREAD") {
			runner.resumeCtxErr = ctx.Err()
		}
		return nil, nil
	}
	if slices.Contains(args, "--version") {
		return []byte("mariadb-backup based on MariaDB server 12.3.3-MariaDB\n"), nil
	}
	directory := argumentValue(args, "--target-dir=")
	if slices.Contains(args, "--backup") {
		if runner.cancelBackup != nil {
			runner.cancelBackup()
			return nil, ctx.Err()
		}
		if runner.failBackup {
			return nil, errors.New("password=must-never-escape")
		}
		if !runner.omitMetadata {
			if err := os.WriteFile(filepath.Join(directory, "mariadb_backup_checkpoints"), []byte("backup_type = full-backuped\n"), 0o600); err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(directory, "mariadb_backup_slave_info"), []byte("SET GLOBAL gtid_slave_pos='1-2-8';\n"), 0o600); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	if slices.Contains(args, "--prepare") {
		if !runner.omitMetadata && !runner.leaveUnprepared {
			if err := os.WriteFile(filepath.Join(directory, "mariadb_backup_checkpoints"), []byte("backup_type = log-applied\n"), 0o600); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	return nil, errors.New("unexpected command")
}

func TestCaptureRejectsUnpreparedBackup(t *testing.T) {
	target := testTarget(t)
	target.Capture.UseMemory = "512M"
	workRoot := t.TempDir()
	_, err := NewCapturerWithRunner(&captureRunner{leaveUnprepared: true}).Capture(context.Background(), target, workRoot)
	if err == nil || !strings.Contains(err.Error(), "not fully prepared") {
		t.Fatalf("Capture(unprepared) error = %v", err)
	}
	entries, readErr := os.ReadDir(workRoot)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("unprepared capture left work files: entries=%v error=%v", entries, readErr)
	}
}

func TestCaptureUsesCleanupContextToResumeAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	runner := &captureRunner{cancelBackup: cancel}
	target := testTarget(t)
	target.Capture.UseMemory = "512M"
	_, err := NewCapturerWithRunner(runner).Capture(ctx, target, t.TempDir())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Capture(cancelled) error = %v", err)
	}
	if runner.resumeCtxErr != nil {
		t.Fatalf("resume used cancelled context: %v", runner.resumeCtxErr)
	}
	if !hasCall(runner.calls, clientPath, "--execute=START REPLICA SQL_THREAD") {
		t.Fatalf("replica was not resumed after cancellation: %#v", runner.calls)
	}
}

func TestCapturePreparesBackupAndResumesReplica(t *testing.T) {
	runner := &captureRunner{}
	target := testTarget(t)
	target.Capture.Prepare = true
	target.Capture.SafeReplicaBackup = true
	target.Capture.UseMemory = "512M"
	workRoot := t.TempDir()

	capture, err := NewCapturerWithRunner(runner).Capture(context.Background(), target, workRoot)
	if err != nil {
		t.Fatalf("Capture() error = %v", err)
	}
	path := capture.Path()
	if capture.GTIDPosition != "1-2-8" || capture.Report.ServerVersion != "12.3.3" {
		t.Fatalf("Capture() result = %#v", capture)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("capture directory mode: info=%v error=%v", info, err)
	}
	if !hasCall(runner.calls, backupPath, "--backup", "--slave-info", "--safe-slave-backup") {
		t.Fatalf("backup call missing safety options: %#v", runner.calls)
	}
	for _, call := range runner.calls {
		if len(call) > 1 && call[0] == backupPath && slices.Contains(call, "--backup") && !strings.HasPrefix(call[1], "--defaults-extra-file=") {
			t.Fatalf("credential option was not first in backup argv: %#v", call)
		}
	}
	if !hasCall(runner.calls, clientPath, "--execute=START REPLICA SQL_THREAD") {
		t.Fatalf("replica resume call missing: %#v", runner.calls)
	}
	if !hasCall(runner.calls, backupPath, "--prepare", "--use-memory=512M") {
		t.Fatalf("prepare call missing: %#v", runner.calls)
	}
	if err := capture.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("capture directory remains after Close(): %v", err)
	}
}

func TestCaptureFailureResumesReplicaRedactsAndCleans(t *testing.T) {
	runner := &captureRunner{failBackup: true}
	target := testTarget(t)
	target.Capture.UseMemory = "512M"
	workRoot := t.TempDir()

	_, err := NewCapturerWithRunner(runner).Capture(context.Background(), target, workRoot)
	if err == nil || strings.Contains(err.Error(), "must-never-escape") {
		t.Fatalf("Capture() error was not safely redacted: %v", err)
	}
	if !hasCall(runner.calls, clientPath, "--execute=START REPLICA SQL_THREAD") {
		t.Fatalf("replica was not resumed after failure: %#v", runner.calls)
	}
	entries, readErr := os.ReadDir(workRoot)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("failed capture left work files: entries=%v error=%v", entries, readErr)
	}
}

func TestCaptureRejectsMissingMetadataAndCleans(t *testing.T) {
	target := testTarget(t)
	target.Capture.UseMemory = "512M"
	workRoot := t.TempDir()
	_, err := NewCapturerWithRunner(&captureRunner{omitMetadata: true}).Capture(context.Background(), target, workRoot)
	if err == nil || !strings.Contains(err.Error(), "checkpoint metadata") {
		t.Fatalf("Capture() error = %v", err)
	}
	entries, readErr := os.ReadDir(workRoot)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("invalid capture left work files: entries=%v error=%v", entries, readErr)
	}
}

func TestCaptureRejectsSymlinkWorkRoot(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(t.TempDir(), "work")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	_, err := NewCapturerWithRunner(&captureRunner{}).Capture(context.Background(), testTarget(t), link)
	if err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("Capture(symlink work root) error = %v", err)
	}
}

func hasArgumentPrefix(args []string, prefix string) bool {
	for _, argument := range args {
		if strings.HasPrefix(argument, prefix) {
			return true
		}
	}
	return false
}

func argumentValue(args []string, prefix string) string {
	for _, argument := range args {
		if strings.HasPrefix(argument, prefix) {
			return strings.TrimPrefix(argument, prefix)
		}
	}
	return ""
}

func hasCall(calls [][]string, executable string, required ...string) bool {
	for _, call := range calls {
		if len(call) == 0 || call[0] != executable {
			continue
		}
		matches := true
		for _, argument := range required {
			if !slices.Contains(call[1:], argument) {
				matches = false
			}
		}
		if matches {
			return true
		}
	}
	return false
}
