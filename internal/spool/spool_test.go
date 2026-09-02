package spool

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/bionicman/savetoa/internal/agecrypto"
	"github.com/bionicman/savetoa/internal/localstore"
	"github.com/bionicman/savetoa/internal/manifest"
)

func TestStageAndIdempotentLocalDelivery(t *testing.T) {
	spool := newTestSpool(t)
	destination := newTestDestination(t)
	payload := []byte("captured once")

	staged, err := spool.Stage(context.Background(), "test", validManifest(), bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("Stage() error = %v", err)
	}
	if _, err := spool.Verify(context.Background(), staged.RelativePath); err != nil {
		t.Fatalf("Verify(staged) error = %v", err)
	}

	delivered, err := spool.DeliverLocal(context.Background(), staged.RelativePath, destination)
	if err != nil {
		t.Fatalf("DeliverLocal() error = %v", err)
	}
	if delivered.RelativePath != staged.RelativePath || delivered.Manifest.Artifact != staged.Manifest.Artifact {
		t.Fatalf("delivered set differs from staged set: %#v", delivered)
	}
	if _, err := destination.Verify(context.Background(), delivered.RelativePath); err != nil {
		t.Fatalf("Verify(delivered) error = %v", err)
	}

	retried, err := spool.DeliverLocal(context.Background(), staged.RelativePath, destination)
	if err != nil {
		t.Fatalf("DeliverLocal(idempotent retry) error = %v", err)
	}
	if retried.Manifest.Artifact.Checksum != staged.Manifest.Artifact.Checksum {
		t.Fatal("idempotent retry changed the artifact")
	}
}

func TestStageEncryptedAndDeliverLocal(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	encryptor, err := agecrypto.ParseRecipients(strings.NewReader(identity.Recipient().String() + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	spool := newTestSpool(t)
	destination := newTestDestination(t)
	plaintext := []byte("encrypted backup payload")

	staged, err := spool.StageEncrypted(context.Background(), "test", validManifest(), bytes.NewReader(plaintext), encryptor)
	if err != nil {
		t.Fatalf("StageEncrypted() error = %v", err)
	}
	if !strings.HasSuffix(staged.Manifest.Artifact.Filename, ".age") {
		t.Fatalf("encrypted filename = %q", staged.Manifest.Artifact.Filename)
	}
	if len(staged.Manifest.Transformations) != 1 || staged.Manifest.Transformations[0].Driver != "age" {
		t.Fatalf("transformations = %#v", staged.Manifest.Transformations)
	}
	assertDecryptsTo(t, spool.store, staged.RelativePath, identity, plaintext)

	delivered, err := spool.DeliverLocal(context.Background(), staged.RelativePath, destination)
	if err != nil {
		t.Fatalf("DeliverLocal(encrypted) error = %v", err)
	}
	assertDecryptsTo(t, destination, delivered.RelativePath, identity, plaintext)
}

func TestDeliveryRejectsTamperedSpoolPayload(t *testing.T) {
	rootPath := t.TempDir()
	spool, err := New(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = spool.Close() })
	destinationPath := t.TempDir()
	destination, err := localstore.New(destinationPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = destination.Close() })

	staged, err := spool.Stage(context.Background(), "test", validManifest(), strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	payloadPath := filepath.Join(rootPath, staged.RelativePath, staged.Manifest.Artifact.Filename)
	if err := os.WriteFile(payloadPath, []byte("payloae"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = spool.DeliverLocal(context.Background(), staged.RelativePath, destination)
	if !errors.Is(err, localstore.ErrArtifactMismatch) {
		t.Fatalf("DeliverLocal(tampered) error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(destinationPath, staged.RelativePath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tampered payload was published: %v", err)
	}
}

func TestDeliveryRejectsConflictingDestinationSet(t *testing.T) {
	spool := newTestSpool(t)
	destination := newTestDestination(t)
	staged, err := spool.Stage(context.Background(), "test", validManifest(), strings.NewReader("source payload"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := destination.Commit(context.Background(), "test", validManifest(), strings.NewReader("different payload")); err != nil {
		t.Fatal(err)
	}
	if _, err := spool.DeliverLocal(context.Background(), staged.RelativePath, destination); !errors.Is(err, ErrDestinationConflict) {
		t.Fatalf("DeliverLocal(conflict) error = %v", err)
	}
}

func TestIdempotentDeliveryStillVerifiesStagedPayload(t *testing.T) {
	rootPath := t.TempDir()
	spool, err := New(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = spool.Close() })
	destination := newTestDestination(t)
	staged, err := spool.Stage(context.Background(), "test", validManifest(), strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spool.DeliverLocal(context.Background(), staged.RelativePath, destination); err != nil {
		t.Fatal(err)
	}
	payloadPath := filepath.Join(rootPath, staged.RelativePath, staged.Manifest.Artifact.Filename)
	if err := os.WriteFile(payloadPath, []byte("payloae"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := spool.DeliverLocal(context.Background(), staged.RelativePath, destination); err == nil || !strings.Contains(err.Error(), "verify staged capture") {
		t.Fatalf("DeliverLocal(tampered retry) error = %v", err)
	}
}

func newTestSpool(t *testing.T) *Spool {
	t.Helper()
	spool, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := spool.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return spool
}

func newTestDestination(t *testing.T) *localstore.Store {
	t.Helper()
	destination, err := localstore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := destination.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return destination
}

func assertDecryptsTo(
	t *testing.T,
	store *localstore.Store,
	relativePath string,
	identity age.Identity,
	want []byte,
) {
	t.Helper()
	_, payload, err := store.OpenPayload(relativePath)
	if err != nil {
		t.Fatal(err)
	}
	defer payload.Close()
	decrypted, err := age.Decrypt(payload, identity)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(decrypted)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("decrypted payload = %q, want %q", got, want)
	}
}

func validManifest() manifest.Manifest {
	return manifest.Manifest{
		FormatVersion: manifest.FormatVersion,
		BackupID:      "20260902-example",
		Target:        "example-mariadb",
		CaptureDriver: "mariadb",
		StartedAt:     time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC),
		CompletedAt:   time.Date(2026, 9, 2, 10, 5, 0, 0, time.UTC),
		Tool:          manifest.Tool{Name: "mariadb-backup", Version: "12.3.0"},
		Source: manifest.Source{
			ServerVersion: "12.3.0",
			Replication:   map[string]string{"gtid": "0-1-42"},
		},
		Artifact:        manifest.Artifact{Filename: "payload.tar"},
		Transformations: []manifest.Transformation{},
	}
}
