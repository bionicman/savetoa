// Package sqlite3 captures a consistent SQLite database through the native CLI.
package sqlite3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bionicman/savetoa/internal/config"
)

const (
	executablePath   = "/usr/bin/sqlite3"
	capturedFilename = "database.sqlite3"
)

var versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:\.[0-9]+)?$`)

type Runner interface {
	Version(context.Context) (string, error)
	QuickCheck(context.Context, string) error
	Backup(context.Context, string, string) error
}

type ProcessRunner struct{}

type Report struct {
	Version string
}

type Capture struct {
	directory string
	workRoot  string
	Report    Report
}

type Capturer struct {
	runner Runner
}

func NewCapturer() *Capturer {
	return &Capturer{runner: ProcessRunner{}}
}

func NewCapturerWithRunner(runner Runner) *Capturer {
	return &Capturer{runner: runner}
}

func (capturer *Capturer) Check(ctx context.Context, target config.Target) (*Report, error) {
	if ctx == nil {
		return nil, errors.New("context is required")
	}
	if capturer == nil || capturer.runner == nil {
		return nil, errors.New("SQLite capturer has no command runner")
	}
	if _, err := inspectSource(target.Source.Path); err != nil {
		return nil, err
	}
	version, err := capturer.runner.Version(ctx)
	if err != nil {
		return nil, commandFailure(ctx, "inspect SQLite version")
	}
	if err := capturer.runner.QuickCheck(ctx, target.Source.Path); err != nil {
		return nil, commandFailure(ctx, "SQLite source quick check")
	}
	return &Report{Version: version}, nil
}

func (capturer *Capturer) Capture(ctx context.Context, target config.Target, workRoot string) (*Capture, error) {
	if ctx == nil {
		return nil, errors.New("context is required")
	}
	if capturer == nil || capturer.runner == nil {
		return nil, errors.New("SQLite capturer has no command runner")
	}
	if err := validateWorkRoot(workRoot); err != nil {
		return nil, err
	}
	sourceBefore, err := inspectSource(target.Source.Path)
	if err != nil {
		return nil, err
	}
	sourceHandle, err := os.Open(target.Source.Path)
	if err != nil {
		return nil, errors.New("open SQLite source identity")
	}
	defer sourceHandle.Close()
	sourceIdentity, err := sourceHandle.Stat()
	if err != nil {
		return nil, errors.New("inspect open SQLite source identity")
	}
	if !os.SameFile(sourceBefore, sourceIdentity) {
		return nil, errors.New("SQLite source was replaced before capture")
	}
	report, err := capturer.Check(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("SQLite pre-capture check: %w", err)
	}

	directory, err := os.MkdirTemp(workRoot, ".sqlite3-")
	if err != nil {
		return nil, errors.New("create SQLite work directory")
	}
	remove := true
	defer func() {
		if remove {
			_ = os.RemoveAll(directory)
		}
	}()
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, errors.New("protect SQLite work directory")
	}
	if err := capturer.runner.Backup(ctx, target.Source.Path, directory); err != nil {
		return nil, commandFailure(ctx, "capture SQLite backup")
	}
	capturedPath := filepath.Join(directory, capturedFilename)
	if err := validateCapturedDatabase(capturedPath); err != nil {
		return nil, err
	}
	if err := os.Chmod(capturedPath, 0o600); err != nil {
		return nil, errors.New("protect captured SQLite database")
	}
	if err := syncFile(capturedPath); err != nil {
		return nil, err
	}
	capturedCheckPath, err := filepath.EvalSymlinks(capturedPath)
	if err != nil {
		return nil, errors.New("resolve captured SQLite database")
	}
	if err := capturer.runner.QuickCheck(ctx, capturedCheckPath); err != nil {
		return nil, commandFailure(ctx, "SQLite captured database quick check")
	}
	sourceAfter, err := inspectSource(target.Source.Path)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(sourceIdentity, sourceAfter) {
		return nil, errors.New("SQLite source was replaced during capture")
	}
	if err := syncDirectory(directory); err != nil {
		return nil, err
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
	if filepath.Dir(directory) != capture.workRoot || !strings.HasPrefix(filepath.Base(directory), ".sqlite3-") {
		return errors.New("refusing to remove unexpected SQLite work directory")
	}
	if err := os.RemoveAll(directory); err != nil {
		return errors.New("remove SQLite work directory")
	}
	return nil
}

func (ProcessRunner) Version(ctx context.Context) (string, error) {
	command := exec.CommandContext(ctx, executablePath, "-version")
	command.Env = commandEnvironment()
	command.Stderr = io.Discard
	output, err := command.Output()
	if err != nil {
		return "", commandFailure(ctx, "inspect SQLite version")
	}
	fields := strings.Fields(string(output))
	if len(fields) == 0 || !versionPattern.MatchString(fields[0]) {
		return "", errors.New("SQLite version output is not recognized")
	}
	return fields[0], nil
}

func (ProcessRunner) QuickCheck(ctx context.Context, databasePath string) error {
	command := quickCheckCommand(ctx, databasePath)
	output, err := command.Output()
	if err != nil {
		return commandFailure(ctx, "run SQLite quick check")
	}
	if strings.TrimSpace(string(output)) != "ok" {
		return errors.New("SQLite quick check failed")
	}
	return nil
}

func (ProcessRunner) Backup(ctx context.Context, sourcePath, destinationDirectory string) error {
	command := backupCommand(ctx, sourcePath, destinationDirectory)
	if err := command.Run(); err != nil {
		return commandFailure(ctx, "run SQLite online backup")
	}
	destinationPath, err := filepath.EvalSymlinks(filepath.Join(destinationDirectory, capturedFilename))
	if err != nil {
		return errors.New("resolve SQLite backup for journal mode normalization")
	}
	command = normalizeCommand(ctx, destinationPath)
	output, err := command.Output()
	if err != nil {
		return commandFailure(ctx, "normalize SQLite backup journal mode")
	}
	if strings.TrimSpace(string(output)) != "delete" {
		return errors.New("SQLite backup journal mode normalization failed")
	}
	return nil
}

func quickCheckCommand(ctx context.Context, databasePath string) *exec.Cmd {
	command := exec.CommandContext(ctx, executablePath,
		"-batch", "-bail", "-readonly", "-nofollow", "-cmd", ".timeout 5000", "--",
		databasePath, "PRAGMA quick_check;")
	command.Env = commandEnvironment()
	command.Stderr = io.Discard
	return command
}

func backupCommand(ctx context.Context, sourcePath, destinationDirectory string) *exec.Cmd {
	command := exec.CommandContext(ctx, executablePath,
		"-batch", "-bail", "-readonly", "-nofollow", "-cmd", ".timeout 5000", "--",
		sourcePath, ".backup "+capturedFilename)
	command.Dir = destinationDirectory
	command.Env = commandEnvironment()
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	return command
}

func normalizeCommand(ctx context.Context, databasePath string) *exec.Cmd {
	command := exec.CommandContext(ctx, executablePath,
		"-batch", "-bail", "-nofollow", "-cmd", ".timeout 5000", "--",
		databasePath, "PRAGMA journal_mode=DELETE;")
	command.Env = commandEnvironment()
	command.Stderr = io.Discard
	return command
}

func inspectSource(path string) (os.FileInfo, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || path == string(filepath.Separator) {
		return nil, errors.New("SQLite source must be a clean absolute path other than root")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("resolve SQLite source: %w", err)
	}
	if resolved != path {
		return nil, errors.New("SQLite source path must not traverse symlinks")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect SQLite source: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("SQLite source must be a regular file, not a symlink")
	}
	return info, nil
}

func validateCapturedDatabase(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return errors.New("inspect captured SQLite database")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() <= 0 {
		return errors.New("captured SQLite database must be a non-empty regular file")
	}
	return nil
}

func validateWorkRoot(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || path == string(filepath.Separator) {
		return errors.New("SQLite work root must be a clean absolute path other than root")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect SQLite work root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("SQLite work root must be a real directory")
	}
	return nil
}

func syncFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return errors.New("open captured SQLite database for sync")
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil {
		return errors.New("persist captured SQLite database")
	}
	if closeErr != nil {
		return errors.New("close captured SQLite database")
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return errors.New("open SQLite work directory for sync")
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return errors.New("persist SQLite work directory")
	}
	if closeErr != nil {
		return errors.New("close SQLite work directory")
	}
	return nil
}

func commandEnvironment() []string {
	return []string{"HOME=/nonexistent", "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "PATH=/usr/bin:/bin"}
}

func commandFailure(ctx context.Context, operation string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return errors.New(operation + " failed")
}
