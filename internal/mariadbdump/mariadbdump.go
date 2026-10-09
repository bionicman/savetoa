// Package mariadbdump captures one MariaDB database as a consistent logical SQL dump.
package mariadbdump

import (
	"bytes"
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
	clientPath = "/usr/bin/mariadb"
	dumpPath   = "/usr/bin/mariadb-dump"
	dumpName   = "database.sql"
)

var versionPattern = regexp.MustCompile(`\b([0-9]+\.[0-9]+\.[0-9]+)(?:-MariaDB)?\b`)

type Report struct {
	ServerVersion string
	ToolVersion   string
}

type Capture struct {
	directory string
	workRoot  string
	Report    Report
}

func (capture *Capture) Path() string { return capture.directory }

func (capture *Capture) Close() error {
	if capture == nil || capture.directory == "" {
		return nil
	}
	directory := capture.directory
	capture.directory = ""
	if filepath.Dir(directory) != capture.workRoot || !strings.HasPrefix(filepath.Base(directory), ".mariadb-dump-") {
		return errors.New("refusing to remove unexpected MariaDB dump work directory")
	}
	if err := os.RemoveAll(directory); err != nil {
		return errors.New("remove MariaDB dump work directory")
	}
	return nil
}

type Runner interface {
	Version(context.Context) (string, error)
	Probe(context.Context, config.Target) (string, error)
	Capture(context.Context, config.Target, string) error
}

type Capturer struct{ runner Runner }

func NewCapturer() *Capturer                        { return &Capturer{runner: ProcessRunner{}} }
func NewCapturerWithRunner(runner Runner) *Capturer { return &Capturer{runner: runner} }

func (capturer *Capturer) Check(ctx context.Context, target config.Target) (*Report, error) {
	if ctx == nil || capturer == nil || capturer.runner == nil {
		return nil, errors.New("context and MariaDB dump runner are required")
	}
	if target.Driver != "mariadb-dump" {
		return nil, errors.New("unsupported MariaDB dump capture mode")
	}
	if err := inspectCredentialFile(target.Credentials.File); err != nil {
		return nil, err
	}
	toolVersion, err := capturer.runner.Version(ctx)
	if err != nil {
		return nil, failure(ctx, "inspect mariadb-dump version")
	}
	serverVersion, err := capturer.runner.Probe(ctx, target)
	if err != nil {
		return nil, failure(ctx, "probe MariaDB source")
	}
	if toolVersion == "" || serverVersion == "" {
		return nil, errors.New("MariaDB server or dump-tool version is missing")
	}
	if toolVersion != serverVersion {
		return nil, fmt.Errorf("MariaDB server version %s does not match mariadb-dump version %s", serverVersion, toolVersion)
	}
	return &Report{ServerVersion: serverVersion, ToolVersion: toolVersion}, nil
}

func (capturer *Capturer) Capture(ctx context.Context, target config.Target, workRoot string) (*Capture, error) {
	if err := inspectWorkRoot(workRoot); err != nil {
		return nil, err
	}
	report, err := capturer.Check(ctx, target)
	if err != nil {
		return nil, err
	}
	directory, err := os.MkdirTemp(workRoot, ".mariadb-dump-")
	if err != nil {
		return nil, errors.New("create MariaDB dump work directory")
	}
	remove := true
	defer func() {
		if remove {
			_ = os.RemoveAll(directory)
		}
	}()
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, errors.New("protect MariaDB dump work directory")
	}
	dumpFile := filepath.Join(directory, dumpName)
	if err := capturer.runner.Capture(ctx, target, dumpFile); err != nil {
		return nil, failure(ctx, "capture MariaDB logical dump")
	}
	if err := validateDump(dumpFile); err != nil {
		return nil, err
	}
	if err := syncPath(dumpFile); err != nil {
		return nil, err
	}
	if err := syncPath(directory); err != nil {
		return nil, err
	}
	remove = false
	return &Capture{directory: directory, workRoot: workRoot, Report: *report}, nil
}

type ProcessRunner struct{}

func (ProcessRunner) Version(ctx context.Context) (string, error) {
	command := exec.CommandContext(ctx, dumpPath, "--version")
	command.Env = baseEnvironment()
	command.Stderr = io.Discard
	output, err := command.Output()
	if err != nil {
		return "", err
	}
	return extractVersion(string(output))
}

func (ProcessRunner) Probe(ctx context.Context, target config.Target) (string, error) {
	command := exec.CommandContext(ctx, clientPath, probeArgs(target)...)
	command.Env = baseEnvironment()
	command.Stderr = io.Discard
	output, err := command.Output()
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(output))
	if value == "" || strings.ContainsAny(value, "\r\n\x00") {
		return "", errors.New("unrecognized MariaDB probe result")
	}
	return extractVersion(value)
}

func (ProcessRunner) Capture(ctx context.Context, target config.Target, outputPath string) error {
	output, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, dumpPath, dumpArgs(target)...)
	command.Env = baseEnvironment()
	command.Stdout = output
	command.Stderr = io.Discard
	runErr := command.Run()
	syncErr := output.Sync()
	closeErr := output.Close()
	if runErr != nil {
		return runErr
	}
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func probeArgs(target config.Target) []string {
	return []string{
		"--defaults-extra-file=" + target.Credentials.File,
		"--protocol=SOCKET",
		"--socket=" + target.Source.Socket,
		"--batch",
		"--skip-column-names",
		"--raw",
		"--database=" + target.Source.Database,
		"--execute=SELECT VERSION()",
	}
}

func dumpArgs(target config.Target) []string {
	return []string{
		"--defaults-extra-file=" + target.Credentials.File,
		"--protocol=SOCKET",
		"--socket=" + target.Source.Socket,
		"--single-transaction",
		"--quick",
		"--routines",
		"--events",
		"--triggers",
		"--hex-blob",
		"--skip-dump-date",
		"--default-character-set=utf8mb4",
		"--databases",
		target.Source.Database,
	}
}

func validateDump(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() == 0 {
		return errors.New("MariaDB logical dump is not a non-empty regular mode-0600 file")
	}
	const marker = "\n-- Dump completed\n"
	readSize := int64(4096)
	if info.Size() < readSize {
		readSize = info.Size()
	}
	file, err := os.Open(path)
	if err != nil {
		return errors.New("open MariaDB logical dump for validation")
	}
	defer file.Close()
	data := make([]byte, readSize)
	if _, err := file.ReadAt(data, info.Size()-readSize); err != nil && !errors.Is(err, io.EOF) {
		return errors.New("read MariaDB logical dump for validation")
	}
	if !bytes.HasSuffix(data, []byte(marker)) {
		return errors.New("MariaDB logical dump has no completion marker")
	}
	return nil
}

func inspectCredentialFile(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("MariaDB credential file must have a clean absolute path")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return errors.New("inspect MariaDB credential file")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() == 0 {
		return errors.New("MariaDB credential file must be a non-empty regular mode-0600 file")
	}
	return nil
}

func inspectWorkRoot(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return errors.New("MariaDB dump work root must be a clean absolute non-root path")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("MariaDB dump work root must be a real directory")
	}
	return nil
}

func extractVersion(value string) (string, error) {
	match := versionPattern.FindStringSubmatch(value)
	if len(match) != 2 {
		return "", errors.New("unrecognized MariaDB version")
	}
	return match[1], nil
}

func syncPath(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return errors.New("open MariaDB dump path for sync")
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil || closeErr != nil {
		return errors.New("persist MariaDB logical dump")
	}
	return nil
}

func baseEnvironment() []string {
	return []string{"HOME=/nonexistent", "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "PATH=/usr/bin:/bin"}
}

func failure(ctx context.Context, operation string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return errors.New(operation + " failed")
}
