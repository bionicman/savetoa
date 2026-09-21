// Package postgresql captures PostgreSQL physical and logical backups with native tools.
package postgresql

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/bionicman/savetoa/internal/config"
)

var versionPattern = regexp.MustCompile(`PostgreSQL\) ([0-9]+(?:\.[0-9]+){0,2})(?:\s|$)`)

type Report struct {
	ServerVersion string
	ToolVersion   string
	ToolName      string
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
	if filepath.Dir(directory) != capture.workRoot || !strings.HasPrefix(filepath.Base(directory), ".postgresql-") {
		return errors.New("refusing to remove unexpected PostgreSQL work directory")
	}
	if err := os.RemoveAll(directory); err != nil {
		return errors.New("remove PostgreSQL work directory")
	}
	return nil
}

type Runner interface {
	Version(context.Context, string) (string, error)
	Probe(context.Context, config.Target) (string, bool, error)
	Capture(context.Context, config.Target, string) error
	Verify(context.Context, config.Target, string) error
}

type Capturer struct{ runner Runner }

func NewCapturer() *Capturer                        { return &Capturer{runner: ProcessRunner{}} }
func NewCapturerWithRunner(runner Runner) *Capturer { return &Capturer{runner: runner} }

func (capturer *Capturer) Check(ctx context.Context, target config.Target) (*Report, error) {
	if ctx == nil || capturer == nil || capturer.runner == nil {
		return nil, errors.New("context and PostgreSQL runner are required")
	}
	if target.Driver != "postgresql-base" && target.Driver != "postgresql-dump" {
		return nil, errors.New("unsupported PostgreSQL capture mode")
	}
	if err := inspectPassfile(target.Credentials.File); err != nil {
		return nil, err
	}
	tool := "pg_basebackup"
	if target.Driver == "postgresql-dump" {
		tool = "pg_dump"
	}
	version, err := capturer.runner.Version(ctx, tool)
	if err != nil {
		return nil, failure(ctx, "inspect PostgreSQL tool version")
	}
	serverVersion, standby, err := capturer.runner.Probe(ctx, target)
	if err != nil {
		return nil, failure(ctx, "probe PostgreSQL source")
	}
	if target.Driver == "postgresql-base" && !standby {
		return nil, errors.New("postgresql-base source is not a standby")
	}
	if serverVersion == "" || version == "" {
		return nil, errors.New("PostgreSQL version is missing")
	}
	if target.Driver == "postgresql-base" {
		serverMajor, serverErr := leadingMajor(serverVersion)
		toolMajor, toolErr := leadingMajor(version)
		if serverErr != nil || toolErr != nil || toolMajor < 17 || serverMajor != toolMajor {
			return nil, errors.New("postgresql-base requires matching server and client major versions, at least 17")
		}
	}
	return &Report{ServerVersion: serverVersion, ToolVersion: version, ToolName: tool}, nil
}

func leadingMajor(version string) (int, error) {
	part := strings.SplitN(version, ".", 2)[0]
	if part == "" || len(part) > 2 {
		return 0, errors.New("invalid PostgreSQL version")
	}
	return strconv.Atoi(part)
}

func (capturer *Capturer) Capture(ctx context.Context, target config.Target, workRoot string) (*Capture, error) {
	if err := inspectWorkRoot(workRoot); err != nil {
		return nil, err
	}
	report, err := capturer.Check(ctx, target)
	if err != nil {
		return nil, err
	}
	directory, err := os.MkdirTemp(workRoot, ".postgresql-")
	if err != nil {
		return nil, errors.New("create PostgreSQL work directory")
	}
	remove := true
	defer func() {
		if remove {
			_ = os.RemoveAll(directory)
		}
	}()
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, errors.New("protect PostgreSQL work directory")
	}
	if err := capturer.runner.Capture(ctx, target, directory); err != nil {
		return nil, failure(ctx, "capture PostgreSQL backup")
	}
	if err := capturer.runner.Verify(ctx, target, directory); err != nil {
		return nil, failure(ctx, "verify PostgreSQL backup")
	}
	if target.Driver == "postgresql-base" {
		version, standby, err := capturer.runner.Probe(ctx, target)
		if err != nil || !standby || version != report.ServerVersion {
			return nil, errors.New("PostgreSQL standby changed during base backup")
		}
	}
	if target.Driver == "postgresql-dump" {
		if err := checkRegular(filepath.Join(directory, "database.dump")); err != nil {
			return nil, err
		}
	} else {
		if err := checkRegular(filepath.Join(directory, "base.tar")); err != nil {
			return nil, err
		}
		if err := checkRegular(filepath.Join(directory, "pg_wal.tar")); err != nil {
			return nil, err
		}
		if err := checkRegular(filepath.Join(directory, "backup_manifest")); err != nil {
			return nil, err
		}
	}
	if err := syncTree(directory); err != nil {
		return nil, err
	}
	remove = false
	return &Capture{directory: directory, workRoot: workRoot, Report: *report}, nil
}

type ProcessRunner struct{}

func (ProcessRunner) Version(ctx context.Context, tool string) (string, error) {
	path := executable(tool)
	if path == "" {
		return "", errors.New("unsupported PostgreSQL tool")
	}
	command := exec.CommandContext(ctx, path, "--version")
	command.Env = baseEnvironment()
	command.Stderr = io.Discard
	output, err := command.Output()
	if err != nil {
		return "", err
	}
	match := versionPattern.FindStringSubmatch(strings.TrimSpace(string(output)))
	if len(match) != 2 {
		return "", errors.New("unrecognized PostgreSQL tool version")
	}
	return match[1], nil
}

func (ProcessRunner) Probe(ctx context.Context, target config.Target) (string, bool, error) {
	command := exec.CommandContext(ctx, executable("psql"), "-X", "-A", "-t", "-w", "-v", "ON_ERROR_STOP=1", "-c", "SELECT current_setting('server_version'), pg_is_in_recovery()")
	command.Env = connectionEnvironment(target, "postgres")
	command.Stderr = io.Discard
	output, err := command.Output()
	if err != nil {
		return "", false, err
	}
	parts := strings.Split(strings.TrimSpace(string(output)), "|")
	if len(parts) != 2 || (parts[1] != "t" && parts[1] != "f") || len(parts[0]) > 80 || strings.ContainsAny(parts[0], "\r\n\x00") {
		return "", false, errors.New("unrecognized PostgreSQL probe result")
	}
	return parts[0], parts[1] == "t", nil
}

func (ProcessRunner) Capture(ctx context.Context, target config.Target, directory string) error {
	command := captureCommand(ctx, target, directory)
	command.Stdout, command.Stderr = io.Discard, io.Discard
	return command.Run()
}

func captureCommand(ctx context.Context, target config.Target, directory string) *exec.Cmd {
	var command *exec.Cmd
	if target.Driver == "postgresql-base" {
		command = exec.CommandContext(ctx, executable("pg_basebackup"), "--format=tar", "--wal-method=stream", "--manifest-checksums=SHA256", "--no-password", "--pgdata="+directory)
		command.Env = connectionEnvironment(target, "replication")
	} else {
		command = exec.CommandContext(ctx, executable("pg_dump"), "--format=custom", "--no-password", "--file="+filepath.Join(directory, "database.dump"))
		command.Env = connectionEnvironment(target, target.Source.Database)
	}
	return command
}

func (ProcessRunner) Verify(ctx context.Context, target config.Target, directory string) error {
	var command *exec.Cmd
	if target.Driver == "postgresql-base" {
		version, err := (ProcessRunner{}).Version(ctx, "pg_basebackup")
		if err != nil {
			return err
		}
		major, err := leadingMajor(version)
		if err != nil || major < 17 || major > 99 {
			return errors.New("unsupported PostgreSQL verifier version")
		}
		command = verificationCommand(ctx, target, directory, major)
	} else {
		command = verificationCommand(ctx, target, directory, 0)
	}
	command.Stdout, command.Stderr = io.Discard, io.Discard
	return command.Run()
}

func verificationCommand(ctx context.Context, target config.Target, directory string, major int) *exec.Cmd {
	var command *exec.Cmd
	if target.Driver == "postgresql-base" {
		command = exec.CommandContext(ctx, fmt.Sprintf("/usr/lib/postgresql/%d/bin/pg_verifybackup", major), "--format=tar", "--no-parse-wal", directory)
	} else {
		command = exec.CommandContext(ctx, executable("pg_restore"), "--file=/dev/null", filepath.Join(directory, "database.dump"))
	}
	command.Env = baseEnvironment()
	return command
}

func executable(name string) string {
	switch name {
	case "psql", "pg_basebackup", "pg_dump", "pg_restore":
		return "/usr/bin/" + name
	default:
		return ""
	}
}

func baseEnvironment() []string {
	return []string{"HOME=/nonexistent", "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "PATH=/usr/bin:/bin"}
}

func connectionEnvironment(target config.Target, database string) []string {
	return append(baseEnvironment(),
		"PGHOST="+target.Source.Host, fmt.Sprintf("PGPORT=%d", target.Source.Port),
		"PGUSER="+target.Source.Username, "PGDATABASE="+database,
		"PGPASSFILE="+target.Credentials.File, "PGCONNECT_TIMEOUT=10", "PGAPPNAME=savetoa")
}

func inspectPassfile(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("PostgreSQL passfile must have a clean absolute path")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return errors.New("inspect PostgreSQL passfile")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() == 0 {
		return errors.New("PostgreSQL passfile must be a non-empty regular mode-0600 file")
	}
	return nil
}

func inspectWorkRoot(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return errors.New("PostgreSQL work root must be a clean absolute non-root path")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("PostgreSQL work root must be a real directory")
	}
	return nil
}

func checkRegular(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return errors.New("PostgreSQL backup is missing a regular non-empty artifact")
	}
	return nil
}

func syncTree(root string) error {
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 || (!entry.IsDir() && !entry.Type().IsRegular()) {
			return errors.New("PostgreSQL backup contains an unsafe file")
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		syncErr := file.Sync()
		closeErr := file.Close()
		if syncErr != nil {
			return syncErr
		}
		return closeErr
	})
	if err != nil {
		return errors.New("persist PostgreSQL backup")
	}
	return nil
}

func failure(ctx context.Context, operation string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return errors.New(operation + " failed")
}
