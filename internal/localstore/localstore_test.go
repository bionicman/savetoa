package localstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bionicman/savetoa/internal/manifest"
)

func TestCommitCreatesDurableVerifiableSet(t *testing.T) {
	store, rootPath := newTestStore(t)
	payload := []byte("durable payload")

	set, err := store.Commit(context.Background(), "test", validManifest(), bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	wantPath := filepath.Join("test", "example-mariadb", "2026", "09", "02", "20260902-example")
	if set.RelativePath != wantPath {
		t.Fatalf("RelativePath = %q, want %q", set.RelativePath, wantPath)
	}
	digest := sha256.Sum256(payload)
	if set.Manifest.Artifact.SizeBytes != int64(len(payload)) || set.Manifest.Artifact.Checksum.Value != hex.EncodeToString(digest[:]) {
		t.Fatalf("artifact metadata = %#v", set.Manifest.Artifact)
	}

	loaded, err := store.Load(set.RelativePath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.Manifest.BackupID != set.Manifest.BackupID {
		t.Fatalf("Load() backup ID = %q", loaded.Manifest.BackupID)
	}
	if _, err := store.Verify(context.Background(), set.RelativePath); err != nil {
		t.Fatalf("Verify() error = %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(rootPath, set.RelativePath))
	if err != nil {
		t.Fatal(err)
	}
	wantFiles := map[string]bool{"complete": true, "manifest.json": true, "payload.tar": true}
	for _, entry := range entries {
		if !wantFiles[entry.Name()] {
			t.Fatalf("unexpected backup-set entry %q", entry.Name())
		}
		delete(wantFiles, entry.Name())
	}
	if len(wantFiles) != 0 {
		t.Fatalf("missing backup-set entries: %v", wantFiles)
	}
}

func TestListCompletedIgnoresIncompleteSetsAndDeleteRemovesValidSet(t *testing.T) {
	store, rootPath := newTestStore(t)
	set, err := store.Commit(context.Background(), "test", validManifest(), strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	incomplete := filepath.Join(rootPath, "test", "example-mariadb", "2026", "09", "03", "20260903-incomplete")
	if err := os.MkdirAll(incomplete, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(incomplete, manifestFilename), []byte("not complete"), 0o600); err != nil {
		t.Fatal(err)
	}
	sets, err := store.ListCompleted("test", "example-mariadb")
	if err != nil {
		t.Fatal(err)
	}
	if len(sets) != 1 || sets[0].RelativePath != set.RelativePath {
		t.Fatalf("completed sets = %#v", sets)
	}
	if err := store.DeleteCompleted(context.Background(), sets[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(rootPath, set.RelativePath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted set still exists: %v", err)
	}
	if _, err := os.Stat(incomplete); err != nil {
		t.Fatalf("incomplete set was touched: %v", err)
	}
}

func TestListCompletedFailsClosedOnInvalidMarker(t *testing.T) {
	store, rootPath := newTestStore(t)
	set, err := store.Commit(context.Background(), "test", validManifest(), strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, set.RelativePath, completeFilename), []byte("invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListCompleted("test", "example-mariadb"); err == nil {
		t.Fatal("invalid completed set was accepted")
	}
	if _, err := os.Stat(filepath.Join(rootPath, set.RelativePath, manifestFilename)); err != nil {
		t.Fatalf("failed scan modified set: %v", err)
	}
}

func TestDeleteCompletedInvalidatesMarkerBeforeCleanupFailure(t *testing.T) {
	store, rootPath := newTestStore(t)
	set, err := store.Commit(context.Background(), "test", validManifest(), strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, set.RelativePath, "unexpected"), []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteCompleted(context.Background(), *set); err == nil {
		t.Fatal("cleanup with an unexpected file succeeded")
	}
	if _, err := store.Load(set.RelativePath); err == nil {
		t.Fatal("partially deleted set remained complete")
	}
	if data, err := os.ReadFile(filepath.Join(rootPath, set.RelativePath, "unexpected")); err != nil || string(data) != "preserve" {
		t.Fatalf("unexpected file was modified: %q, %v", data, err)
	}
}

func TestCommitNeverOverwritesExistingSet(t *testing.T) {
	store, _ := newTestStore(t)
	value := validManifest()
	firstPayload := []byte("first payload")
	set, err := store.Commit(context.Background(), "test", value, bytes.NewReader(firstPayload))
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.Commit(context.Background(), "test", value, strings.NewReader("replacement"))
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("second Commit() error = %v", err)
	}
	verified, err := store.Verify(context.Background(), set.RelativePath)
	if err != nil {
		t.Fatalf("Verify(original) error = %v", err)
	}
	digest := sha256.Sum256(firstPayload)
	if verified.Manifest.Artifact.Checksum.Value != hex.EncodeToString(digest[:]) {
		t.Fatal("existing payload was replaced")
	}
}

func TestCommitWithExpectedMetadataRejectsMismatchAndCanRetry(t *testing.T) {
	source, _ := newTestStore(t)
	destination, destinationPath := newTestStore(t)
	payload := []byte("captured payload")
	staged, err := source.Commit(context.Background(), "test", validManifest(), bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}

	_, err = destination.Commit(context.Background(), "test", staged.Manifest, strings.NewReader("changed payload"))
	if !errors.Is(err, ErrArtifactMismatch) {
		t.Fatalf("Commit(mismatch) error = %v", err)
	}
	finalPath := filepath.Join(destinationPath, staged.RelativePath)
	if _, err := os.Stat(finalPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mismatched artifact was published: %v", err)
	}

	retried, err := destination.Commit(context.Background(), "test", staged.Manifest, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("Commit(retry) error = %v", err)
	}
	if _, err := destination.Verify(context.Background(), retried.RelativePath); err != nil {
		t.Fatalf("Verify(retry) error = %v", err)
	}
}

func TestConcurrentCommitAllowsOneWriter(t *testing.T) {
	store, _ := newTestStore(t)
	value := validManifest()

	var wait sync.WaitGroup
	errorsSeen := make(chan error, 2)
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := store.Commit(context.Background(), "test", value, strings.NewReader("payload"))
			errorsSeen <- err
		}()
	}
	wait.Wait()
	close(errorsSeen)

	successes := 0
	failures := 0
	for err := range errorsSeen {
		if err == nil {
			successes++
		} else if strings.Contains(err.Error(), "already exists") {
			failures++
		} else {
			t.Fatalf("unexpected Commit() error = %v", err)
		}
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("successes = %d, failures = %d", successes, failures)
	}
}

func TestFailedAndCancelledCommitsHaveNoCompletionMarker(t *testing.T) {
	tests := map[string]struct {
		context func() context.Context
		reader  func(context.CancelFunc) io.Reader
	}{
		"reader failure": {
			context: func() context.Context { return context.Background() },
			reader: func(context.CancelFunc) io.Reader {
				return &failingReader{}
			},
		},
		"cancellation": {
			context: func() context.Context { return context.Background() },
			reader: func(cancel context.CancelFunc) io.Reader {
				return &cancellingReader{cancel: cancel}
			},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			store, rootPath := newTestStore(t)
			baseContext := test.context()
			ctx, cancel := context.WithCancel(baseContext)
			defer cancel()
			_, err := store.Commit(ctx, "test", validManifest(), test.reader(cancel))
			if err == nil {
				t.Fatal("Commit() error = nil")
			}

			setPath := filepath.Join(rootPath, "test", "example-mariadb", "2026", "09", "02", "20260902-example")
			if _, statErr := os.Stat(filepath.Join(setPath, completeFilename)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("completion marker exists after failed commit: %v", statErr)
			}
			if _, loadErr := store.Load(filepath.Join("test", "example-mariadb", "2026", "09", "02", "20260902-example")); loadErr == nil {
				t.Fatal("Load(incomplete set) error = nil")
			}
			dayPath := filepath.Join(rootPath, "test", "example-mariadb", "2026", "09", "02")
			entries, readErr := os.ReadDir(dayPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 0 {
				t.Fatalf("failed commit left partial entries: %v", entries)
			}
		})
	}
}

func TestEmptyPayloadDoesNotCompleteSet(t *testing.T) {
	store, rootPath := newTestStore(t)
	_, err := store.Commit(context.Background(), "test", validManifest(), bytes.NewReader(nil))
	if err == nil || !strings.Contains(err.Error(), "payload is empty") {
		t.Fatalf("Commit(empty) error = %v", err)
	}
	marker := filepath.Join(rootPath, "test", "example-mariadb", "2026", "09", "02", "20260902-example", completeFilename)
	if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("completion marker exists for empty payload: %v", statErr)
	}
}

func TestInvalidManifestDoesNotCreateSet(t *testing.T) {
	store, rootPath := newTestStore(t)
	value := validManifest()
	value.Transformations = nil
	if _, err := store.Commit(context.Background(), "test", value, strings.NewReader("payload")); err == nil {
		t.Fatal("Commit(invalid manifest) error = nil")
	}
	if _, err := os.Stat(filepath.Join(rootPath, "test")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid commit created hierarchy: %v", err)
	}
}

func TestLoadAndVerifyDetectTampering(t *testing.T) {
	t.Run("manifest", func(t *testing.T) {
		store, rootPath := newTestStore(t)
		set, err := store.Commit(context.Background(), "test", validManifest(), strings.NewReader("payload"))
		if err != nil {
			t.Fatal(err)
		}
		manifestPath := filepath.Join(rootPath, set.RelativePath, manifestFilename)
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(manifestPath, append(data, ' '), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Load(set.RelativePath); err == nil || !strings.Contains(err.Error(), "does not match manifest") {
			t.Fatalf("Load(tampered manifest) error = %v", err)
		}
	})

	t.Run("payload", func(t *testing.T) {
		store, rootPath := newTestStore(t)
		set, err := store.Commit(context.Background(), "test", validManifest(), strings.NewReader("payload"))
		if err != nil {
			t.Fatal(err)
		}
		payloadPath := filepath.Join(rootPath, set.RelativePath, set.Manifest.Artifact.Filename)
		if err := os.WriteFile(payloadPath, []byte("payloae"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Verify(context.Background(), set.RelativePath); err == nil || !strings.Contains(err.Error(), "checksum") {
			t.Fatalf("Verify(tampered payload) error = %v", err)
		}
	})

	t.Run("marker", func(t *testing.T) {
		store, rootPath := newTestStore(t)
		set, err := store.Commit(context.Background(), "test", validManifest(), strings.NewReader("payload"))
		if err != nil {
			t.Fatal(err)
		}
		markerPath := filepath.Join(rootPath, set.RelativePath, completeFilename)
		if err := os.WriteFile(markerPath, []byte("complete\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Load(set.RelativePath); err == nil || !strings.Contains(err.Error(), "marker is invalid") {
			t.Fatalf("Load(tampered marker) error = %v", err)
		}
	})
}

func TestCommitRejectsSymlinkedHierarchy(t *testing.T) {
	rootPath := t.TempDir()
	outsidePath := t.TempDir()
	if err := os.Symlink(outsidePath, filepath.Join(rootPath, "test")); err != nil {
		t.Fatal(err)
	}
	store, err := New(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if _, err := store.Commit(context.Background(), "test", validManifest(), strings.NewReader("payload")); err == nil || !strings.Contains(err.Error(), "not a real directory") {
		t.Fatalf("Commit(symlink hierarchy) error = %v", err)
	}
	entries, err := os.ReadDir(outsidePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("commit escaped destination root: %v", entries)
	}
}

func TestNewAndLoadRejectUnsafePaths(t *testing.T) {
	if _, err := New("relative"); err == nil {
		t.Fatal("New(relative) error = nil")
	}
	if _, err := New(string(filepath.Separator)); err == nil {
		t.Fatal("New(root) error = nil")
	}
	store, _ := newTestStore(t)
	if _, err := store.Load("../outside"); err == nil {
		t.Fatal("Load(traversal) error = nil")
	}
}

func TestFindByIDFindsOnlyCompletedUniqueSet(t *testing.T) {
	store, rootPath := newTestStore(t)
	set, err := store.Commit(context.Background(), "test", validManifest(), strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}

	found, err := store.FindByID(set.Manifest.BackupID)
	if err != nil {
		t.Fatalf("FindByID() error = %v", err)
	}
	if found.RelativePath != set.RelativePath {
		t.Fatalf("FindByID() path = %q, want %q", found.RelativePath, set.RelativePath)
	}
	if _, err := store.FindByID("missing"); !errors.Is(err, ErrSetNotFound) {
		t.Fatalf("FindByID(missing) error = %v", err)
	}

	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "target", "2026", "09", "02", set.Manifest.BackupID), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(rootPath, "linked")); err != nil {
		t.Fatal(err)
	}
	found, err = store.FindByID(set.Manifest.BackupID)
	if err != nil || found.RelativePath != set.RelativePath {
		t.Fatalf("FindByID(with symlink) = %#v, %v", found, err)
	}
}

func TestFindByIDRejectsAmbiguousID(t *testing.T) {
	store, _ := newTestStore(t)
	value := validManifest()
	if _, err := store.Commit(context.Background(), "first", value, strings.NewReader("payload")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit(context.Background(), "second", value, strings.NewReader("payload")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FindByID(value.BackupID); !errors.Is(err, ErrAmbiguousSet) {
		t.Fatalf("FindByID(ambiguous) error = %v", err)
	}
}

func TestFindByIDIgnoresIncompleteSet(t *testing.T) {
	store, rootPath := newTestStore(t)
	value := validManifest()
	incomplete := filepath.Join(rootPath, "test", value.Target, "2026", "09", "02", value.BackupID)
	if err := os.MkdirAll(incomplete, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FindByID(value.BackupID); !errors.Is(err, ErrSetNotFound) {
		t.Fatalf("FindByID(incomplete) error = %v", err)
	}
}

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	rootPath := t.TempDir()
	store, err := New(rootPath)
	if err != nil {
		t.Fatalf("New(%q) error = %v", rootPath, err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return store, rootPath
}

func validManifest() manifest.Manifest {
	return manifest.Manifest{
		FormatVersion: manifest.FormatVersion,
		BackupID:      "20260902-example",
		Target:        "example-mariadb",
		CaptureDriver: "mariadb",
		StartedAt:     time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC),
		CompletedAt:   time.Date(2026, 9, 2, 10, 5, 0, 0, time.UTC),
		Tool: manifest.Tool{
			Name:    "mariadb-backup",
			Version: "12.3.0",
		},
		Source: manifest.Source{
			ServerVersion: "12.3.0",
			Replication:   map[string]string{"gtid": "0-1-42"},
		},
		Artifact: manifest.Artifact{
			Filename: "payload.tar",
		},
		Transformations: []manifest.Transformation{},
	}
}

type failingReader struct {
	read bool
}

func (reader *failingReader) Read(buffer []byte) (int, error) {
	if reader.read {
		return 0, errors.New("injected read failure")
	}
	reader.read = true
	return copy(buffer, "partial"), errors.New("injected read failure")
}

type cancellingReader struct {
	cancel context.CancelFunc
	read   bool
}

func (reader *cancellingReader) Read(buffer []byte) (int, error) {
	if reader.read {
		return 0, io.EOF
	}
	reader.read = true
	n := copy(buffer, "partial")
	reader.cancel()
	return n, nil
}
