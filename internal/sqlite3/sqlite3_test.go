package sqlite3

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bionicman/savetoa/internal/config"
)

type fakeRunner struct {
	version         string
	versionErr      error
	quickCheckErr   error
	backupErr       error
	backupPayload   []byte
	skipBackupWrite bool
	replaceSource   bool
	quickCheckPath  []string
	backupSource    string
	backupDir       string
}

func (runner *fakeRunner) Version(context.Context) (string, error) {
	return runner.version, runner.versionErr
}

func (runner *fakeRunner) QuickCheck(_ context.Context, path string) error {
	runner.quickCheckPath = append(runner.quickCheckPath, path)
	return runner.quickCheckErr
}

func (runner *fakeRunner) Backup(_ context.Context, source, directory string) error {
	runner.backupSource = source
	runner.backupDir = directory
	if runner.backupErr != nil {
		return runner.backupErr
	}
	if runner.skipBackupWrite {
		return nil
	}
	if runner.replaceSource {
		if err := os.Remove(source); err != nil {
			return err
		}
		if err := os.WriteFile(source, []byte("replacement database"), 0o600); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(directory, capturedFilename), runner.backupPayload, 0o666)
}

func TestCheckValidatesSourceAndDatabase(t *testing.T) {
	source := writeSource(t)
	runner := &fakeRunner{version: "3.46.1"}
	report, err := NewCapturerWithRunner(runner).Check(context.Background(), sqliteTarget(source))
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if report.Version != "3.46.1" || !slices.Equal(runner.quickCheckPath, []string{source}) {
		t.Fatalf("report=%#v quick checks=%v", report, runner.quickCheckPath)
	}
}

func TestCheckRejectsUnsafeSources(t *testing.T) {
	root := t.TempDir()
	regular := filepath.Join(root, "database.sqlite3")
	if err := os.WriteFile(regular, []byte("sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(root, "database-link.sqlite3")
	if err := os.Symlink(regular, symlink); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"relative":  "database.sqlite3",
		"directory": root,
		"symlink":   symlink,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewCapturerWithRunner(&fakeRunner{version: "3.46.1"}).Check(context.Background(), sqliteTarget(path))
			if err == nil {
				t.Fatal("Check(unsafe source) succeeded")
			}
		})
	}
}

func TestCaptureCreatesProtectedVerifiedSnapshotAndCleansIt(t *testing.T) {
	source := writeSource(t)
	workRoot := t.TempDir()
	runner := &fakeRunner{version: "3.46.1", backupPayload: []byte("captured database")}
	capture, err := NewCapturerWithRunner(runner).Capture(context.Background(), sqliteTarget(source), workRoot)
	if err != nil {
		t.Fatalf("Capture() error = %v", err)
	}
	capturedPath := filepath.Join(capture.Path(), capturedFilename)
	capturedCheckPath, err := filepath.EvalSymlinks(capturedPath)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(capturedPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("captured mode = %o, want 600", info.Mode().Perm())
	}
	if runner.backupSource != source || runner.backupDir != capture.Path() {
		t.Fatalf("backup source=%q dir=%q", runner.backupSource, runner.backupDir)
	}
	if !slices.Equal(runner.quickCheckPath, []string{source, capturedCheckPath}) {
		t.Fatalf("quick checks = %v", runner.quickCheckPath)
	}
	directory := capture.Path()
	if err := capture.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("work directory remains after close: %v", err)
	}
}

func TestCaptureRemovesPartialSnapshotOnFailure(t *testing.T) {
	source := writeSource(t)
	workRoot := t.TempDir()
	runner := &fakeRunner{version: "3.46.1", backupErr: errors.New("native details")}
	_, err := NewCapturerWithRunner(runner).Capture(context.Background(), sqliteTarget(source), workRoot)
	if err == nil || strings.Contains(err.Error(), "native details") {
		t.Fatalf("Capture() error = %v", err)
	}
	entries, readErr := os.ReadDir(workRoot)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("partial work entries = %v", entries)
	}
}

func TestCaptureRejectsMissingOrEmptySnapshot(t *testing.T) {
	for name, test := range map[string]struct {
		payload []byte
		skip    bool
	}{
		"missing": {skip: true},
		"empty":   {payload: []byte{}},
	} {
		t.Run(name, func(t *testing.T) {
			source := writeSource(t)
			workRoot := t.TempDir()
			runner := &fakeRunner{version: "3.46.1", backupPayload: test.payload, skipBackupWrite: test.skip}
			_, err := NewCapturerWithRunner(runner).Capture(context.Background(), sqliteTarget(source), workRoot)
			if err == nil {
				t.Fatal("Capture(invalid snapshot) succeeded")
			}
		})
	}
}

func TestCaptureRejectsSourceReplacement(t *testing.T) {
	source := writeSource(t)
	runner := &fakeRunner{version: "3.46.1", backupPayload: []byte("captured"), replaceSource: true}
	_, err := NewCapturerWithRunner(runner).Capture(context.Background(), sqliteTarget(source), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Fatalf("Capture(replaced source) error = %v", err)
	}
}

func TestNativeCommandsKeepConfiguredPathOutOfCommandText(t *testing.T) {
	ctx := context.Background()
	source := "/srv/application/database with spaces.sqlite3"
	directory := "/var/lib/savetoa/work/.sqlite3-fixed"
	check := quickCheckCommand(ctx, source)
	if check.Path != executablePath || check.Args[len(check.Args)-2] != source || check.Args[len(check.Args)-1] != "PRAGMA quick_check;" {
		t.Fatalf("quick-check command = %#v", check.Args)
	}
	backup := backupCommand(ctx, source, directory)
	if backup.Path != executablePath || backup.Dir != directory || backup.Args[len(backup.Args)-2] != source || backup.Args[len(backup.Args)-1] != ".backup database.sqlite3" {
		t.Fatalf("backup command path=%q dir=%q args=%#v", backup.Path, backup.Dir, backup.Args)
	}
	if strings.Contains(backup.Args[len(backup.Args)-1], source) || strings.Contains(backup.Args[len(backup.Args)-1], directory) {
		t.Fatal("configured path entered SQLite command text")
	}
	normalize := normalizeCommand(ctx, filepath.Join(directory, capturedFilename))
	if normalize.Path != executablePath || normalize.Args[len(normalize.Args)-1] != "PRAGMA journal_mode=DELETE;" {
		t.Fatalf("normalize command = %#v", normalize.Args)
	}
}

func TestVersionOutputValidation(t *testing.T) {
	for _, value := range []string{"3.46.1", "3.46.1.0"} {
		if !versionPattern.MatchString(value) {
			t.Fatalf("version %q was rejected", value)
		}
	}
	for _, value := range []string{"", "3.46", "sqlite 3.46.1", "3.46.1\nsecret"} {
		if versionPattern.MatchString(value) {
			t.Fatalf("invalid version %q was accepted", value)
		}
	}
}

func TestProcessRunnerCapturesSQLiteDatabase(t *testing.T) {
	if _, err := os.Stat(executablePath); err != nil {
		t.Skipf("%s is unavailable: %v", executablePath, err)
	}
	root := t.TempDir()
	source := filepath.Join(root, "source database.sqlite3")
	if err := os.WriteFile(source, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := filepath.EvalSymlinks(source)
	if err != nil {
		t.Fatal(err)
	}
	writerContext, cancelWriter := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelWriter()
	writer := exec.CommandContext(writerContext, executablePath, "-batch", "-bail", "--", source)
	writer.Env = commandEnvironment()
	stdin, err := writer.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := writer.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var writerStderr bytes.Buffer
	writer.Stderr = &writerStderr
	if err := writer.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stdin.Close()
		_ = writer.Wait()
	}()
	commands := "PRAGMA journal_mode=WAL;\nPRAGMA wal_autocheckpoint=0;\n" +
		"CREATE TABLE records(value TEXT);\nINSERT INTO records VALUES('captured from WAL');\n.print ready\n"
	if _, err := io.WriteString(stdin, commands); err != nil {
		t.Fatal(err)
	}
	ready := false
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		if scanner.Text() == "ready" {
			ready = true
			break
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if !ready {
		t.Fatalf("SQLite WAL writer did not become ready: %s", writerStderr.String())
	}
	walInfo, err := os.Stat(source + "-wal")
	if err != nil || walInfo.Size() == 0 {
		t.Fatalf("SQLite WAL was not created: info=%v error=%v", walInfo, err)
	}
	workRoot := filepath.Join(root, "work with spaces")
	if err := os.Mkdir(workRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	capture, err := NewCapturer().Capture(context.Background(), sqliteTarget(source), workRoot)
	if err != nil {
		t.Fatalf("Capture(real SQLite) error = %v", err)
	}
	defer capture.Close()
	query := exec.Command(executablePath, "-batch", "-bail", "-readonly", "--",
		filepath.Join(capture.Path(), capturedFilename), "SELECT value FROM records;")
	query.Env = commandEnvironment()
	output, err := query.Output()
	if err != nil {
		t.Fatalf("query captured SQLite database: %v", err)
	}
	if strings.TrimSpace(string(output)) != "captured from WAL" {
		t.Fatalf("captured value = %q", output)
	}
}

func writeSource(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "database.sqlite3")
	if err := os.WriteFile(path, []byte("source database"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func sqliteTarget(path string) config.Target {
	return config.Target{Driver: "sqlite3", Source: config.MariaDBSource{Path: path}}
}
