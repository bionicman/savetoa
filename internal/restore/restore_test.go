package restore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/bionicman/savetoa/internal/agecrypto"
	archivepkg "github.com/bionicman/savetoa/internal/archive"
	"github.com/bionicman/savetoa/internal/localstore"
	"github.com/bionicman/savetoa/internal/manifest"
)

func TestMaterializeEncryptedCompressedBackup(t *testing.T) {
	ctx := context.Background()
	input := t.TempDir()
	if err := os.WriteFile(filepath.Join(input, "ibdata1"), []byte("prepared database pages"), 0o600); err != nil {
		t.Fatal(err)
	}
	compressed, err := archivepkg.TarZstdReader(ctx, input, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer compressed.Close()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	encryptor, err := agecrypto.ParseRecipients(strings.NewReader(identity.Recipient().String()))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := encryptor.EncryptReader(ctx, compressed)
	if err != nil {
		t.Fatal(err)
	}
	level := 3
	value := testManifest()
	value.Artifact.Filename = "payload.tar.zst.age"
	value.Transformations = []manifest.Transformation{{Driver: "zstd", Level: &level}, encryptor.Transformation()}
	sourceRoot := t.TempDir()
	store, err := localstore.New(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	set, err := store.Commit(ctx, "test", value, payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	identityFile := filepath.Join(t.TempDir(), "restore.age")
	if err := os.WriteFile(identityFile, []byte(identity.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	restored, err := Materialize(ctx, Options{SourceRoot: sourceRoot, BackupID: set.Manifest.BackupID, TargetDir: target, IdentityFile: identityFile})
	if err != nil {
		t.Fatal(err)
	}
	if restored.RelativePath != set.RelativePath {
		t.Fatalf("restored set path = %q, want %q", restored.RelativePath, set.RelativePath)
	}
	data, err := os.ReadFile(filepath.Join(target, "ibdata1"))
	if err != nil || string(data) != "prepared database pages" {
		t.Fatalf("restored payload = %q, error = %v", data, err)
	}
}

func TestMaterializeRejectsUnsafeIdentityModeBeforeCreatingTarget(t *testing.T) {
	identityFile := filepath.Join(t.TempDir(), "restore.age")
	if err := os.WriteFile(identityFile, []byte("private"), 0o644); err != nil {
		t.Fatal(err)
	}
	identities, err := readIdentities(identityFile)
	if err == nil || identities != nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("readIdentities(0644) = %#v, %v", identities, err)
	}
}

func testManifest() manifest.Manifest {
	return manifest.Manifest{
		FormatVersion: manifest.FormatVersion, BackupID: "20260902-restore", Target: "mariadb",
		CaptureDriver: "mariadb", StartedAt: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC),
		CompletedAt: time.Date(2026, 9, 2, 10, 1, 0, 0, time.UTC),
		Tool:        manifest.Tool{Name: "mariadb-backup", Version: "12.3.3"},
		Source:      manifest.Source{ServerVersion: "12.3.3", Replication: map[string]string{"gtid": "1-1-1"}},
		Artifact:    manifest.Artifact{Filename: "payload.tar"}, Transformations: []manifest.Transformation{},
	}
}
