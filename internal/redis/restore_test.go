package redis

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
	preflight bool
	started   bool
	ready     bool
	stopped   bool
	readyErr  error
}

type fakeDisposableServer struct{ runtime *fakeRestoreRuntime }

func (fakeDisposableServer) Alive() error { return nil }

func (server fakeDisposableServer) Stop(context.Context) error {
	server.runtime.stopped = true
	return nil
}

func (runtime *fakeRestoreRuntime) Preflight(_ context.Context, serverVersion, cliVersion string) error {
	if serverVersion != "8.0.5" || cliVersion != "8.0.5" {
		return errors.New("wrong versions")
	}
	runtime.preflight = true
	return nil
}

func (*fakeRestoreRuntime) ReservePort() (int, error) { return 16379, nil }

func (runtime *fakeRestoreRuntime) Start(_ context.Context, dataDir, logPath string, port int) (DisposableServer, error) {
	data, err := os.ReadFile(filepath.Join(dataDir, "dump.rdb"))
	if err != nil || string(data) != "REDIS0012 test snapshot" || filepath.Base(logPath) != "redis.log" || port == 0 {
		return nil, errors.New("unsafe disposable Redis input")
	}
	runtime.started = true
	return fakeDisposableServer{runtime: runtime}, nil
}

func (runtime *fakeRestoreRuntime) Ready(context.Context, int) error {
	runtime.ready = true
	return runtime.readyErr
}

func TestVerifyRestoreUsesOnlyDisposableLocalRuntime(t *testing.T) {
	sourceRoot, backupID := redisBackupSet(t, false)
	targetDir := filepath.Join(t.TempDir(), "restore")
	runtime := &fakeRestoreRuntime{}
	set, err := VerifyRestore(context.Background(), RestoreOptions{
		SourceRoot: sourceRoot, BackupID: backupID, TargetDir: targetDir,
	}, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if set.Manifest.CaptureDriver != "redis" || !runtime.preflight || !runtime.started || !runtime.ready || !runtime.stopped {
		t.Fatalf("runtime=%#v set=%#v", runtime, set)
	}
	if _, err := os.Stat(filepath.Join(targetDir, "dump.rdb")); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyRestoreFailureRemovesOnlyCreatedTarget(t *testing.T) {
	sourceRoot, backupID := redisBackupSet(t, false)
	targetDir := filepath.Join(t.TempDir(), "failed")
	runtime := &fakeRestoreRuntime{readyErr: errors.New("injected failure")}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := VerifyRestore(ctx, RestoreOptions{
		SourceRoot: sourceRoot, BackupID: backupID, TargetDir: targetDir,
	}, runtime); err == nil {
		t.Fatal("VerifyRestore(failure) succeeded")
	}
	if _, err := os.Stat(targetDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed restore target survived: %v", err)
	}
	if !runtime.stopped {
		t.Fatal("disposable server was not stopped")
	}
}

func TestVerifyRestoreRejectsNonRedisSetBeforeMaterialization(t *testing.T) {
	sourceRoot, backupID := redisBackupSet(t, true)
	targetDir := filepath.Join(t.TempDir(), "restore")
	runtime := &fakeRestoreRuntime{}
	if _, err := VerifyRestore(context.Background(), RestoreOptions{
		SourceRoot: sourceRoot, BackupID: backupID, TargetDir: targetDir,
	}, runtime); err == nil {
		t.Fatal("VerifyRestore(non-Redis) succeeded")
	}
	if runtime.preflight || runtime.started {
		t.Fatal("runtime was used for a non-Redis set")
	}
}

func redisBackupSet(t *testing.T, mongoDB bool) (string, string) {
	t.Helper()
	var payload bytes.Buffer
	writer := tar.NewWriter(&payload)
	data := []byte("REDIS0012 test snapshot")
	if err := writer.WriteHeader(&tar.Header{Name: "dump.rdb", Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
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
	driver, tool, version := "redis", "redis-cli", "8.0.5"
	if mongoDB {
		driver, tool, version = "mongodb", "mongodump", "100.18.0"
	}
	value := manifest.Manifest{
		FormatVersion: manifest.FormatVersion, BackupID: "20260902-redis-test", Target: "example-redis",
		CaptureDriver: driver, StartedAt: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC),
		CompletedAt: time.Date(2026, 9, 2, 10, 1, 0, 0, time.UTC),
		Tool:        manifest.Tool{Name: tool, Version: version},
		Source: manifest.Source{ServerVersion: "8.0.5",
			Replication: map[string]string{"source_host": "redis-primary.internal"}},
		Artifact: manifest.Artifact{Filename: "payload.tar"}, Transformations: []manifest.Transformation{},
	}
	if _, err := store.Commit(context.Background(), "test", value, bytes.NewReader(payload.Bytes())); err != nil {
		t.Fatal(err)
	}
	return root, value.BackupID
}
