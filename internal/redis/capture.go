package redis

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bionicman/savetoa/internal/config"
)

type Capturer struct {
	doctor *Doctor
	client Client
}

type Capture struct {
	directory string
	workRoot  string
	Report    Report
}

func NewCapturer() *Capturer {
	client := CommandClient{}
	return &Capturer{doctor: NewDoctorWithDependencies(client, CommandVersionRunner{}), client: client}
}

func NewCapturerWithDependencies(doctor *Doctor, client Client) *Capturer {
	return &Capturer{doctor: doctor, client: client}
}

func (capturer *Capturer) Capture(ctx context.Context, target config.Target, workRoot string) (*Capture, error) {
	if ctx == nil {
		return nil, errors.New("context is required")
	}
	if capturer == nil || capturer.doctor == nil || capturer.client == nil {
		return nil, errors.New("Redis capturer dependencies are required")
	}
	if err := validateWorkRoot(workRoot); err != nil {
		return nil, err
	}
	preflight, err := capturer.doctor.Check(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("Redis pre-capture health gate: %w", err)
	}
	credentials, err := ReadCredentialsFile(target.Credentials.File)
	if err != nil {
		return nil, err
	}
	maxWait, _ := time.ParseDuration(target.Capture.MaxWait)
	captureContext, cancel := context.WithTimeout(ctx, maxWait)
	defer cancel()
	for time.Now().Unix() <= preflight.LastSave {
		select {
		case <-captureContext.Done():
			return nil, fmt.Errorf("wait to request a newer Redis snapshot: %w", captureContext.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	if err := capturer.client.BGSAVE(captureContext, target, credentials); err != nil {
		return nil, errors.New("request Redis BGSAVE")
	}
	var completed *State
	for {
		state, inspectErr := capturer.client.Inspect(captureContext, target, credentials)
		if inspectErr != nil {
			if captureContext.Err() != nil {
				return nil, fmt.Errorf("wait for Redis BGSAVE: %w", captureContext.Err())
			}
			return nil, errors.New("inspect Redis BGSAVE")
		}
		if !state.BGSAVEInProgress && state.LastSave > preflight.LastSave {
			if state.LastBGSAVEStatus != "ok" {
				return nil, errors.New("Redis BGSAVE did not complete successfully")
			}
			completed = state
			break
		}
		select {
		case <-captureContext.Done():
			return nil, fmt.Errorf("wait for Redis BGSAVE: %w", captureContext.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	directory, err := os.MkdirTemp(workRoot, ".redis-")
	if err != nil {
		return nil, errors.New("create Redis work directory")
	}
	remove := true
	defer func() {
		if remove {
			_ = os.RemoveAll(directory)
		}
	}()
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, errors.New("protect Redis work directory")
	}
	if err := copyRDB(target.Source.RDBFile, filepath.Join(directory, "dump.rdb"), completed.LastSave); err != nil {
		return nil, err
	}
	report, err := capturer.doctor.Check(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("Redis post-capture health gate: %w", err)
	}
	if report.LastSave < completed.LastSave {
		return nil, errors.New("Redis LASTSAVE regressed after capture")
	}
	remove = false
	return &Capture{directory: directory, workRoot: workRoot, Report: *report}, nil
}

func copyRDB(source, destination string, minimumLastSave int64) error {
	before, err := os.Lstat(source)
	if err != nil || before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() || before.Size() <= 0 {
		return errors.New("Redis RDB source must be a non-empty regular file")
	}
	input, err := os.Open(source)
	if err != nil {
		return errors.New("open Redis RDB source")
	}
	defer input.Close()
	opened, err := input.Stat()
	if err != nil || !os.SameFile(before, opened) || opened.ModTime().Unix() < minimumLastSave {
		return errors.New("Redis RDB source does not match the completed BGSAVE")
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("create captured Redis RDB")
	}
	closed := false
	defer func() {
		if !closed {
			_ = output.Close()
		}
	}()
	written, err := io.Copy(output, input)
	if err != nil || written != opened.Size() {
		return errors.New("copy Redis RDB")
	}
	if err := output.Sync(); err != nil {
		return errors.New("sync captured Redis RDB")
	}
	if err := output.Close(); err != nil {
		return errors.New("close captured Redis RDB")
	}
	closed = true
	after, err := os.Lstat(source)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, after) {
		return errors.New("Redis RDB changed identity during capture")
	}
	return nil
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
	if filepath.Dir(directory) != capture.workRoot || !strings.HasPrefix(filepath.Base(directory), ".redis-") {
		return errors.New("refusing to remove unexpected Redis work directory")
	}
	if err := os.RemoveAll(directory); err != nil {
		return errors.New("remove Redis work directory")
	}
	return nil
}

func validateWorkRoot(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || path == string(filepath.Separator) {
		return errors.New("Redis work root must be a clean absolute path other than root")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return errors.New("inspect Redis work root")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("Redis work root must be a real directory")
	}
	return nil
}
