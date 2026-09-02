package mongodb

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bionicman/savetoa/internal/localstore"
	"github.com/bionicman/savetoa/internal/manifest"
)

type fakeRestoreRuntime struct {
	replayError error
	preflight   bool
	started     bool
	replayed    bool
	stopped     bool
}

type fakeDisposableServer struct{ runtime *fakeRestoreRuntime }

func (fakeDisposableServer) Alive() error { return nil }

func (server fakeDisposableServer) Stop(context.Context) error {
	server.runtime.stopped = true
	return nil
}

func (runtime *fakeRestoreRuntime) Preflight(_ context.Context, serverVersion, toolsVersion string) error {
	if serverVersion != "8.3.10" || toolsVersion != "100.18.0" {
		return errors.New("wrong versions")
	}
	runtime.preflight = true
	return nil
}

func (*fakeRestoreRuntime) ReservePort() (int, error) { return 27099, nil }

func (runtime *fakeRestoreRuntime) Start(_ context.Context, dbPath, logPath string, port int) (DisposableServer, error) {
	if filepath.Base(dbPath) != "db" || filepath.Base(logPath) != "mongod.log" || port == 0 {
		return nil, errors.New("unsafe disposable server paths")
	}
	runtime.started = true
	return fakeDisposableServer{runtime: runtime}, nil
}

func (*fakeRestoreRuntime) Ready(context.Context, int) error { return nil }

func (runtime *fakeRestoreRuntime) Replay(_ context.Context, archivePath string, port int) error {
	data, err := os.ReadFile(archivePath)
	if err != nil || string(data) != "full mongo archive" || port == 0 {
		return errors.New("wrong replay input")
	}
	runtime.replayed = true
	return runtime.replayError
}

func TestVerifyRestoreUsesOnlyDisposableLocalRuntime(t *testing.T) {
	sourceRoot, backupID := mongoBackupSet(t, false)
	targetDir := filepath.Join(t.TempDir(), "restore")
	runtime := &fakeRestoreRuntime{}
	set, err := VerifyRestore(context.Background(), RestoreOptions{
		SourceRoot: sourceRoot, BackupID: backupID, TargetDir: targetDir,
	}, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if set.Manifest.CaptureDriver != "mongodb" || !runtime.preflight || !runtime.started || !runtime.replayed || !runtime.stopped {
		t.Fatalf("runtime=%#v set=%#v", runtime, set)
	}
	if _, err := os.Stat(filepath.Join(targetDir, "db")); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyRestoreFailureRemovesOnlyCreatedTarget(t *testing.T) {
	sourceRoot, backupID := mongoBackupSet(t, false)
	targetDir := filepath.Join(t.TempDir(), "failed")
	runtime := &fakeRestoreRuntime{replayError: errors.New("injected failure")}
	if _, err := VerifyRestore(context.Background(), RestoreOptions{SourceRoot: sourceRoot, BackupID: backupID, TargetDir: targetDir}, runtime); err == nil {
		t.Fatal("VerifyRestore(failure) succeeded")
	}
	if _, err := os.Stat(targetDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed restore target survived: %v", err)
	}
	if !runtime.stopped {
		t.Fatal("disposable server was not stopped")
	}
}

func TestVerifyRestoreRejectsNonMongoSetBeforeMaterialization(t *testing.T) {
	sourceRoot, backupID := mongoBackupSet(t, true)
	targetDir := filepath.Join(t.TempDir(), "restore")
	runtime := &fakeRestoreRuntime{}
	if _, err := VerifyRestore(context.Background(), RestoreOptions{SourceRoot: sourceRoot, BackupID: backupID, TargetDir: targetDir}, runtime); err == nil {
		t.Fatal("VerifyRestore(non-MongoDB) succeeded")
	}
	if runtime.preflight || runtime.started {
		t.Fatal("runtime was used for a non-MongoDB set")
	}
}

func mongoBackupSet(t *testing.T, mariaDB bool) (string, string) {
	t.Helper()
	var payload bytes.Buffer
	writer := tar.NewWriter(&payload)
	data := []byte("full mongo archive")
	if err := writer.WriteHeader(&tar.Header{Name: "dump.archive", Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	store, err := localstore.New(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	driver := "mongodb"
	tool := "mongodump"
	if mariaDB {
		driver, tool = "mariadb", "mariadb-backup"
	}
	value := manifest.Manifest{
		FormatVersion: manifest.FormatVersion, BackupID: "20260902-mongodb-test", Target: "example-mongodb",
		CaptureDriver: driver, StartedAt: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC), CompletedAt: time.Date(2026, 9, 2, 10, 1, 0, 0, time.UTC),
		Tool:     manifest.Tool{Name: tool, Version: "100.18.0"},
		Source:   manifest.Source{ServerVersion: "8.3.10", Replication: map[string]string{"set_name": "example-production"}},
		Artifact: manifest.Artifact{Filename: "payload.tar"}, Transformations: []manifest.Transformation{},
	}
	if _, err := store.Commit(context.Background(), "test", value, bytes.NewReader(payload.Bytes())); err != nil {
		t.Fatal(err)
	}
	return root, value.BackupID
}
