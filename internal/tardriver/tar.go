// Package tardriver captures explicit filesystem paths as a tar stream.
package tardriver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bionicman/savetoa/internal/config"
)

const executablePath = "/usr/bin/tar"

type Runner interface {
	Version(context.Context) (string, error)
	Create(context.Context, []string) (io.ReadCloser, error)
}

type ProcessRunner struct{}

type Report struct {
	Version string
	Paths   int
}

type Capture struct {
	file     *os.File
	path     string
	workRoot string
	Report   Report
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
		return nil, errors.New("tar capturer has no command runner")
	}
	if err := inspectSources(ctx, target.Source.Paths); err != nil {
		return nil, err
	}
	version, err := capturer.runner.Version(ctx)
	if err != nil {
		return nil, err
	}
	return &Report{Version: version, Paths: len(target.Source.Paths)}, nil
}

func (capturer *Capturer) Capture(ctx context.Context, target config.Target, workRoot string) (*Capture, error) {
	report, err := capturer.Check(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("tar pre-capture check: %w", err)
	}
	if err := validateWorkRoot(workRoot); err != nil {
		return nil, err
	}
	reader, err := capturer.runner.Create(ctx, target.Source.Paths)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	file, err := os.CreateTemp(workRoot, ".tar-*.tar")
	if err != nil {
		return nil, errors.New("create tar work file")
	}
	path := file.Name()
	remove := true
	defer func() {
		if remove {
			_ = file.Close()
			_ = os.Remove(path)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return nil, errors.New("protect tar work file")
	}
	if _, err := copyContext(ctx, file, reader); err != nil {
		return nil, fmt.Errorf("capture tar stream: %w", err)
	}
	if err := file.Sync(); err != nil {
		return nil, errors.New("persist tar work file")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, errors.New("rewind tar work file")
	}
	remove = false
	return &Capture{file: file, path: path, workRoot: workRoot, Report: *report}, nil
}

func (capture *Capture) Reader() io.Reader {
	if capture == nil {
		return nil
	}
	return capture.file
}

func (capture *Capture) Close() error {
	if capture == nil || capture.file == nil {
		return nil
	}
	file := capture.file
	path := capture.path
	workRoot := capture.workRoot
	capture.file = nil
	capture.path = ""
	closeErr := file.Close()
	if filepath.Dir(path) != workRoot || !strings.HasPrefix(filepath.Base(path), ".tar-") || filepath.Ext(path) != ".tar" {
		return errors.New("refusing to remove unexpected tar work file")
	}
	removeErr := os.Remove(path)
	if closeErr != nil {
		return errors.New("close tar work file")
	}
	if removeErr != nil {
		return errors.New("remove tar work file")
	}
	return nil
}

func (ProcessRunner) Version(ctx context.Context) (string, error) {
	command := exec.CommandContext(ctx, executablePath, "--version")
	command.Env = commandEnvironment()
	command.Stderr = io.Discard
	output, err := command.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("inspect GNU tar version: %w", ctx.Err())
		}
		return "", errors.New("inspect GNU tar version")
	}
	line, _, _ := strings.Cut(string(output), "\n")
	const prefix = "tar (GNU tar) "
	if !strings.HasPrefix(line, prefix) || strings.TrimSpace(strings.TrimPrefix(line, prefix)) == "" {
		return "", errors.New("/usr/bin/tar must be GNU tar")
	}
	return strings.TrimSpace(strings.TrimPrefix(line, prefix)), nil
}

func (ProcessRunner) Create(ctx context.Context, paths []string) (io.ReadCloser, error) {
	if ctx == nil {
		return nil, errors.New("context is required")
	}
	arguments := createArguments(paths)

	reader, writer := io.Pipe()
	command := exec.CommandContext(ctx, executablePath, arguments...)
	command.Env = commandEnvironment()
	command.Stdout = writer
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		_ = reader.Close()
		_ = writer.Close()
		return nil, errors.New("start GNU tar capture")
	}
	go func() {
		err := command.Wait()
		if err != nil {
			if ctx.Err() != nil {
				err = ctx.Err()
			} else {
				err = errors.New("GNU tar capture failed")
			}
		}
		_ = writer.CloseWithError(err)
	}()
	return reader, nil
}

func createArguments(paths []string) []string {
	ordered := append([]string(nil), paths...)
	sort.Strings(ordered)
	arguments := []string{
		"--create",
		"--file=-",
		"--format=posix",
		"--numeric-owner",
		"--hard-dereference",
		"--directory=/",
		"--",
	}
	for _, sourcePath := range ordered {
		arguments = append(arguments, strings.TrimPrefix(sourcePath, string(filepath.Separator)))
	}
	return arguments
}

func commandEnvironment() []string {
	return []string{"LANG=C.UTF-8", "LC_ALL=C.UTF-8", "PATH=/usr/bin:/bin"}
}

func validateWorkRoot(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || path == string(filepath.Separator) {
		return errors.New("tar work root must be a clean absolute path other than root")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect tar work root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("tar work root must be a real directory")
	}
	return nil
}

func copyContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	buffer := make([]byte, 128<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		read, readErr := source.Read(buffer)
		if read > 0 {
			written, writeErr := destination.Write(buffer[:read])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written != read {
				return total, io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}

func inspectSources(ctx context.Context, paths []string) error {
	for _, sourcePath := range paths {
		info, err := os.Lstat(sourcePath)
		if err != nil {
			return fmt.Errorf("inspect tar source: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return errors.New("each tar source must be a real directory or regular file")
		}
		if err := filepath.WalkDir(sourcePath, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				link, err := os.Readlink(path)
				if err != nil {
					return err
				}
				if !safeSourceLink(path, link, paths) {
					return errors.New("tar source contains a symlink outside configured paths")
				}
				return nil
			}
			if !info.IsDir() && !info.Mode().IsRegular() {
				return errors.New("tar source contains an unsupported file type")
			}
			return nil
		}); err != nil {
			return fmt.Errorf("inspect tar source tree: %w", err)
		}
	}
	return nil
}

func safeSourceLink(linkPath, linkTarget string, sources []string) bool {
	if linkTarget == "" || filepath.IsAbs(linkTarget) {
		return false
	}
	resolved := filepath.Clean(filepath.Join(filepath.Dir(linkPath), linkTarget))
	for _, source := range sources {
		if resolved == source || strings.HasPrefix(resolved, source+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
