package archive

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/sys/unix"
)

func TestTarZstdReaderRoundTrip(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "database"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "database", "ibdata1"), []byte("physical backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	compressed, err := TarZstdReader(context.Background(), root, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer compressed.Close()
	decoder, err := zstd.NewReader(compressed)
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	entries := readTar(t, decoder)
	if got := string(entries["database/ibdata1"]); got != "physical backup" {
		t.Fatalf("archived payload = %q", got)
	}
}

func TestTarReaderRejectsSymlinkWithoutReadingTarget(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("must not be read"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	reader, err := TarReader(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("ReadAll(symlink tree) data=%q error=%v", data, err)
	}
}

func TestTarReaderHonorsCancellation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "payload"), bytes.Repeat([]byte("x"), 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reader, err := TarReader(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := io.ReadAll(reader); !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadAll(cancelled) error = %v", err)
	}
}

func TestTarReaderRejectsSpecialFiles(t *testing.T) {
	root := t.TempDir()
	if err := unix.Mkfifo(filepath.Join(root, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := TarReader(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := io.ReadAll(reader); err == nil || !strings.Contains(err.Error(), "unsupported file") {
		t.Fatalf("ReadAll(special file) error = %v", err)
	}
}

func TestExtractTarMaterializesFilesIntoNewTarget(t *testing.T) {
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	if err := writer.WriteHeader(&tar.Header{Name: "mysql/", Typeflag: tar.TypeDir, Mode: 0o750}); err != nil {
		t.Fatal(err)
	}
	data := []byte("database pages")
	if err := writer.WriteHeader(&tar.Header{Name: "mysql/ibdata1", Typeflag: tar.TypeReg, Mode: 0o640, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	target := filepath.Join(parent, "restore")
	if err := ExtractTar(context.Background(), &buffer, target); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(target, "mysql", "ibdata1"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("restored data = %q, error = %v", got, err)
	}
	if err := ExtractTar(context.Background(), bytes.NewReader(nil), target); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("ExtractTar(existing target) error = %v", err)
	}
}

func TestExtractTarRejectsUnsafeEntriesAndRemovesPartialTarget(t *testing.T) {
	tests := map[string]*tar.Header{
		"traversal": {Name: "../escape", Typeflag: tar.TypeReg, Mode: 0o600},
		"absolute":  {Name: "/escape", Typeflag: tar.TypeReg, Mode: 0o600},
		"symlink":   {Name: "link", Typeflag: tar.TypeSymlink, Linkname: "outside", Mode: 0o777},
		"hardlink":  {Name: "link", Typeflag: tar.TypeLink, Linkname: "outside", Mode: 0o600},
	}
	for name, header := range tests {
		t.Run(name, func(t *testing.T) {
			var buffer bytes.Buffer
			writer := tar.NewWriter(&buffer)
			if err := writer.WriteHeader(header); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			parent := t.TempDir()
			target := filepath.Join(parent, "restore")
			if err := ExtractTar(context.Background(), &buffer, target); err == nil {
				t.Fatal("ExtractTar(unsafe) error = nil")
			}
			if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("partial restore remains: %v", err)
			}
		})
	}
}

func readTar(t *testing.T, input io.Reader) map[string][]byte {
	t.Helper()
	result := make(map[string][]byte)
	reader := tar.NewReader(input)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return result
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		result[header.Name] = data
	}
}
