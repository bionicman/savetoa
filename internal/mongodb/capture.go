package mongodb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/bionicman/savetoa/internal/config"
)

type ArchiveRunner interface {
	CaptureArchive(context.Context, string, ...string) error
}

type ArchiveCommandRunner struct{}

func (ArchiveCommandRunner) CaptureArchive(ctx context.Context, name string, args ...string) error {
	command := exec.CommandContext(ctx, name, args...)
	if err := command.Run(); err != nil {
		return fmt.Errorf("%s failed: %w", filepath.Base(name), err)
	}
	return nil
}

type Capturer struct {
	doctor *Doctor
	runner ArchiveRunner
}

type Capture struct {
	directory string
	workRoot  string
	Report    Report
}

func NewCapturer() *Capturer {
	return &Capturer{doctor: NewDoctor(), runner: ArchiveCommandRunner{}}
}

func NewCapturerWithDependencies(doctor *Doctor, runner ArchiveRunner) *Capturer {
	return &Capturer{doctor: doctor, runner: runner}
}

func (capturer *Capturer) Capture(ctx context.Context, target config.Target, workRoot string) (*Capture, error) {
	if ctx == nil {
		return nil, errors.New("context is required")
	}
	if capturer == nil || capturer.doctor == nil || capturer.runner == nil {
		return nil, errors.New("MongoDB capturer dependencies are required")
	}
	if err := validateWorkRoot(workRoot); err != nil {
		return nil, err
	}
	if _, err := capturer.doctor.Check(ctx, target); err != nil {
		return nil, fmt.Errorf("MongoDB pre-capture health gate: %w", err)
	}
	directory, err := os.MkdirTemp(workRoot, ".mongodb-")
	if err != nil {
		return nil, errors.New("create MongoDB work directory")
	}
	remove := true
	defer func() {
		if remove {
			_ = os.RemoveAll(directory)
		}
	}()
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, errors.New("protect MongoDB work directory")
	}
	archivePath := filepath.Join(directory, "dump.archive")
	args := []string{
		"--config=" + target.Credentials.File,
		"--host=" + target.Source.Host,
		"--port=" + strconv.Itoa(target.Source.Port),
		"--username=" + target.Source.Username,
		"--authenticationDatabase=" + target.Source.AuthenticationDatabase,
		"--readPreference=secondary",
		"--archive=" + archivePath,
		"--oplog",
	}
	if err := capturer.runner.CaptureArchive(ctx, dumpPath, args...); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("capture MongoDB archive: %w", ctx.Err())
		}
		return nil, errors.New("capture MongoDB archive")
	}
	info, err := os.Lstat(archivePath)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() == 0 {
		return nil, errors.New("mongodump did not create a non-empty regular archive")
	}
	report, err := capturer.doctor.Check(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("MongoDB post-capture health gate: %w", err)
	}
	remove = false
	return &Capture{directory: directory, workRoot: workRoot, Report: *report}, nil
}

func (capture *Capture) Path() string {
	if capture == nil {
		return ""
	}
	return capture.directory
}

func (capture *Capture) Close() error {
	if capture == nil || capture.directory == "" {
		return nil
	}
	directory := capture.directory
	capture.directory = ""
	if filepath.Dir(directory) != capture.workRoot || !strings.HasPrefix(filepath.Base(directory), ".mongodb-") {
		return errors.New("refusing to remove unexpected MongoDB work directory")
	}
	if err := os.RemoveAll(directory); err != nil {
		return errors.New("remove MongoDB work directory")
	}
	return nil
}

func validateWorkRoot(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || path == string(filepath.Separator) {
		return errors.New("MongoDB work root must be a clean absolute path other than root")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return errors.New("inspect MongoDB work root")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("MongoDB work root must be a real directory")
	}
	return nil
}
