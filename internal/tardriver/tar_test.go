package tardriver

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bionicman/savetoa/internal/config"
	"golang.org/x/sys/unix"
)

type fakeRunner struct {
	version string
	paths   []string
	payload []byte
}

func (runner *fakeRunner) Version(context.Context) (string, error) {
	return runner.version, nil
}

func (runner *fakeRunner) Create(_ context.Context, paths []string) (io.ReadCloser, error) {
	runner.paths = append([]string(nil), paths...)
	return io.NopCloser(bytes.NewReader(runner.payload)), nil
}

func TestCaptureChecksSourcesAndReturnsTarStream(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "certificate.pem"), []byte("certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{version: "1.35", payload: []byte("tar stream")}
	workRoot := t.TempDir()
	capture, err := NewCapturerWithRunner(runner).Capture(context.Background(), config.Target{
		Driver: "tar", Source: config.MariaDBSource{Paths: []string{root}},
	}, workRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer capture.Close()
	data, err := io.ReadAll(capture.Reader())
	if err != nil || string(data) != "tar stream" {
		t.Fatalf("capture payload = %q, error = %v", data, err)
	}
	if capture.Report.Version != "1.35" || !reflect.DeepEqual(runner.paths, []string{root}) {
		t.Fatalf("capture = %#v, paths = %#v", capture, runner.paths)
	}
	if err := capture.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(workRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("tar work file survived close: entries=%v error=%v", entries, err)
	}
}

func TestCheckRejectsUnsafeFilesystemEntries(t *testing.T) {
	tests := map[string]func(string) error{
		"configured symlink": func(root string) error {
			return os.Symlink(t.TempDir(), root)
		},
		"escaping symlink": func(root string) error {
			if err := os.Mkdir(root, 0o700); err != nil {
				return err
			}
			return os.Symlink("../../outside", filepath.Join(root, "escape"))
		},
		"special file": func(root string) error {
			if err := os.Mkdir(root, 0o700); err != nil {
				return err
			}
			return unix.Mkfifo(filepath.Join(root, "pipe"), 0o600)
		},
	}
	for name, prepare := range tests {
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "source")
			if err := prepare(root); err != nil {
				t.Fatal(err)
			}
			_, err := NewCapturerWithRunner(&fakeRunner{version: "1.35"}).Check(context.Background(), config.Target{
				Driver: "tar", Source: config.MariaDBSource{Paths: []string{root}},
			})
			if err == nil || !strings.Contains(err.Error(), "tar source") {
				t.Fatalf("Check(unsafe source) error = %v", err)
			}
		})
	}
}

func TestCheckAcceptsRelativeSymlinkInsideSource(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "archive", "example"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "live", "example"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../archive/example/cert.pem", filepath.Join(root, "live", "example", "cert.pem")); err != nil {
		t.Fatal(err)
	}
	if _, err := NewCapturerWithRunner(&fakeRunner{version: "1.35"}).Check(context.Background(), config.Target{
		Driver: "tar", Source: config.MariaDBSource{Paths: []string{root}},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCreateArgumentsAreFixedAndTerminateOptions(t *testing.T) {
	got := createArguments([]string{"/-leading", "/etc/acme"})
	want := []string{
		"--create", "--file=-", "--format=posix", "--numeric-owner", "--hard-dereference", "--directory=/", "--",
		"-leading", "etc/acme",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("createArguments() = %#v, want %#v", got, want)
	}
}

func TestProcessRunnerProducesRestorableGNUtarStream(t *testing.T) {
	runner := ProcessRunner{}
	if _, err := runner.Version(context.Background()); err != nil {
		t.Skipf("GNU tar is unavailable: %v", err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cert.pem"), []byte("certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("cert.pem", filepath.Join(root, "current.pem")); err != nil {
		t.Fatal(err)
	}
	stream, err := runner.Create(context.Background(), []string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	reader := tar.NewReader(stream)
	var regular, symlink bool
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case strings.HasSuffix(header.Name, "/cert.pem") && header.Typeflag == tar.TypeReg:
			regular = true
		case strings.HasSuffix(header.Name, "/current.pem") && header.Typeflag == tar.TypeSymlink && header.Linkname == "cert.pem":
			symlink = true
		}
	}
	if !regular || !symlink {
		t.Fatalf("GNU tar entries: regular=%t symlink=%t", regular, symlink)
	}

	missing := "/definitely-missing-savetoa-tar-source"
	failed, err := runner.Create(context.Background(), []string{missing})
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(failed)
	_ = failed.Close()
	if err == nil || !strings.Contains(err.Error(), "GNU tar capture failed") || strings.Contains(err.Error(), missing) {
		t.Fatalf("GNU tar failure = %v", err)
	}
}
