// Package hook delivers bounded, non-secret lifecycle events to administrator-managed executables.
package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

const (
	SchemaVersion  = 1
	DefaultRoot    = "/etc/savetoa/hooks.d"
	DefaultTimeout = 15 * time.Second
	maxHooks       = 32
)

var actions = map[string]struct{}{
	"deliver": {},
	"doctor":  {},
	"fetch":   {},
	"prune":   {},
	"run":     {},
	"status":  {},
}

type Event struct {
	SchemaVersion int       `json:"schema_version"`
	Action        string    `json:"action"`
	Outcome       string    `json:"outcome"`
	Environment   string    `json:"environment"`
	Target        string    `json:"target"`
	BackupID      string    `json:"backup_id,omitempty"`
	Repository    string    `json:"repository,omitempty"`
	StartedAt     time.Time `json:"started_at"`
	FinishedAt    time.Time `json:"finished_at"`
	DurationMS    int64     `json:"duration_ms"`
	ExitCode      int       `json:"exit_code"`
}

type Runner struct {
	Root        string
	Timeout     time.Duration
	RequiredUID uint32
}

func DefaultRunner() Runner {
	return Runner{Root: DefaultRoot, Timeout: DefaultTimeout, RequiredUID: 0}
}

func (runner Runner) Run(event Event) error {
	if err := validateEvent(event); err != nil {
		return err
	}
	if runner.Root == "" || !filepath.IsAbs(runner.Root) || filepath.Clean(runner.Root) != runner.Root || runner.Root == string(filepath.Separator) {
		return errors.New("hook root must be a clean absolute path other than the filesystem root")
	}
	if runner.Timeout <= 0 {
		return errors.New("hook timeout must be positive")
	}

	actionDirectory := filepath.Join(runner.Root, event.Action)
	hookDirectory := filepath.Join(actionDirectory, event.Outcome+".d")
	for _, directory := range []string{runner.Root, actionDirectory, hookDirectory} {
		info, err := os.Lstat(directory)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect hook directory: %w", err)
		}
		if err := validateOwnedPath(info, runner.RequiredUID, true); err != nil {
			return fmt.Errorf("unsafe hook directory %q: %w", directory, err)
		}
	}

	entries, err := os.ReadDir(hookDirectory)
	if err != nil {
		return fmt.Errorf("read hook directory: %w", err)
	}
	if len(entries) > maxHooks {
		return fmt.Errorf("hook directory exceeds %d entries", maxHooks)
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode hook event: %w", err)
	}
	payload = append(payload, '\n')

	ctx, cancel := context.WithTimeout(context.Background(), runner.Timeout)
	defer cancel()
	var failures []error
	for _, entry := range entries {
		path := filepath.Join(hookDirectory, entry.Name())
		info, statErr := os.Lstat(path)
		if statErr != nil {
			failures = append(failures, fmt.Errorf("inspect hook %q: %w", entry.Name(), statErr))
			continue
		}
		if validationErr := validateOwnedPath(info, runner.RequiredUID, false); validationErr != nil {
			failures = append(failures, fmt.Errorf("unsafe hook %q: %w", entry.Name(), validationErr))
			continue
		}
		command := exec.CommandContext(ctx, path)
		command.Stdin = bytes.NewReader(payload)
		command.Stdout = io.Discard
		command.Stderr = io.Discard
		command.Env = []string{"LANG=C.UTF-8", "PATH=/usr/bin:/bin"}
		command.Dir = "/"
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		command.Cancel = func() error {
			if command.Process == nil {
				return os.ErrProcessDone
			}
			if killErr := syscall.Kill(-command.Process.Pid, syscall.SIGKILL); errors.Is(killErr, syscall.ESRCH) {
				return os.ErrProcessDone
			} else {
				return killErr
			}
		}
		command.WaitDelay = time.Second
		if runErr := command.Run(); runErr != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				failures = append(failures, fmt.Errorf("hook %q timed out", entry.Name()))
			} else {
				failures = append(failures, fmt.Errorf("hook %q failed: %w", entry.Name(), runErr))
			}
		}
	}
	return errors.Join(failures...)
}

func validateEvent(event Event) error {
	if event.SchemaVersion != SchemaVersion {
		return fmt.Errorf("hook schema_version must be %d", SchemaVersion)
	}
	if _, ok := actions[event.Action]; !ok {
		return fmt.Errorf("unsupported hook action %q", event.Action)
	}
	if event.Outcome != "success" && event.Outcome != "failure" {
		return errors.New("hook outcome must be success or failure")
	}
	if event.Environment == "" || event.Target == "" || event.StartedAt.IsZero() || event.FinishedAt.IsZero() {
		return errors.New("hook environment, target, and timestamps are required")
	}
	if event.FinishedAt.Before(event.StartedAt) || event.DurationMS < 0 {
		return errors.New("hook event timing is invalid")
	}
	return nil
}

func validateOwnedPath(info os.FileInfo, requiredUID uint32, directory bool) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("symbolic links are not allowed")
	}
	if directory && !info.IsDir() {
		return errors.New("path is not a directory")
	}
	if !directory && (!info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0) {
		return errors.New("path is not a regular executable file")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return errors.New("path is writable by group or other")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != requiredUID {
		return fmt.Errorf("path must be owned by uid %d", requiredUID)
	}
	return nil
}
