package mariadb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bionicman/savetoa/internal/config"
)

const metadataFileLimit = 64 << 10

var gtidPattern = regexp.MustCompile(`[0-9]+-[0-9]+-[0-9]+(?:,[0-9]+-[0-9]+-[0-9]+)*`)

type Capturer struct {
	runner Runner
}

type Capture struct {
	directory    string
	workRoot     string
	Report       Report
	GTIDPosition string
}

func NewCapturer() *Capturer {
	return &Capturer{runner: CommandRunner{}}
}

func NewCapturerWithRunner(runner Runner) *Capturer {
	return &Capturer{runner: runner}
}

func (capturer *Capturer) Capture(ctx context.Context, target config.Target, workRoot string) (*Capture, error) {
	if ctx == nil {
		return nil, errors.New("context is required")
	}
	if capturer == nil || capturer.runner == nil {
		return nil, errors.New("MariaDB capturer has no command runner")
	}
	if err := validateWorkRoot(workRoot); err != nil {
		return nil, err
	}
	if _, err := NewDoctorWithRunner(capturer.runner).Check(ctx, target); err != nil {
		return nil, fmt.Errorf("MariaDB pre-capture health gate: %w", err)
	}

	directory, err := os.MkdirTemp(workRoot, ".mariadb-")
	if err != nil {
		return nil, fmt.Errorf("create MariaDB work directory: %w", err)
	}
	remove := true
	defer func() {
		if remove {
			_ = os.RemoveAll(directory)
		}
	}()
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, fmt.Errorf("protect MariaDB work directory: %w", err)
	}

	backupArgs := []string{
		"--defaults-extra-file=" + target.Credentials.File,
		"--backup",
		"--target-dir=" + directory,
		"--slave-info",
		"--safe-slave-backup",
	}
	_, backupErr := capturer.runner.Run(ctx, backupPath, backupArgs...)
	resumeContext, cancelResume := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	resumeErr := capturer.resumeReplica(resumeContext, target)
	cancelResume()
	if backupErr != nil {
		if resumeErr != nil {
			return nil, commandFailure(ctx, "capture MariaDB backup and resume replica SQL thread")
		}
		return nil, commandFailure(ctx, "capture MariaDB backup")
	}
	if resumeErr != nil {
		return nil, resumeErr
	}

	prepareArgs := []string{
		"--prepare",
		"--use-memory=" + target.Capture.UseMemory,
		"--target-dir=" + directory,
	}
	if _, err := capturer.runner.Run(ctx, backupPath, prepareArgs...); err != nil {
		return nil, commandFailure(ctx, "prepare MariaDB backup")
	}
	if err := validatePreparedBackup(directory); err != nil {
		return nil, err
	}
	gtid, err := readBackupGTID(directory)
	if err != nil {
		return nil, err
	}
	report, err := NewDoctorWithRunner(capturer.runner).Check(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("MariaDB post-capture health gate: %w", err)
	}

	remove = false
	return &Capture{directory: directory, workRoot: workRoot, Report: *report, GTIDPosition: gtid}, nil
}

func (capturer *Capturer) resumeReplica(ctx context.Context, target config.Target) error {
	args := []string{
		"--defaults-extra-file=" + target.Credentials.File,
		"--protocol=SOCKET",
		"--socket=" + target.Source.Socket,
		"--batch",
		"--execute=START REPLICA SQL_THREAD",
	}
	if _, err := capturer.runner.Run(ctx, clientPath, args...); err != nil {
		return commandFailure(ctx, "resume MariaDB replica SQL thread")
	}
	return nil
}

func (capture *Capture) Close() error {
	if capture == nil || capture.directory == "" {
		return nil
	}
	directory := capture.directory
	capture.directory = ""
	if filepath.Dir(directory) != capture.workRoot || !strings.HasPrefix(filepath.Base(directory), ".mariadb-") {
		return errors.New("refusing to remove unexpected MariaDB work directory")
	}
	if err := os.RemoveAll(directory); err != nil {
		return fmt.Errorf("remove MariaDB work directory: %w", err)
	}
	return nil
}

func (capture *Capture) Path() string {
	if capture == nil {
		return ""
	}
	return capture.directory
}

func validateWorkRoot(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || path == string(filepath.Separator) {
		return errors.New("MariaDB work root must be a clean absolute path other than root")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect MariaDB work root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("MariaDB work root must be a real directory")
	}
	return nil
}

func validatePreparedBackup(directory string) error {
	for _, name := range []string{"mariadb_backup_checkpoints", "xtrabackup_checkpoints"} {
		data, err := readMetadataFile(directory, name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect MariaDB checkpoint metadata: %w", err)
		}
		if strings.Contains(string(data), "backup_type = log-applied") {
			return nil
		}
		return errors.New("MariaDB backup is not fully prepared")
	}
	return errors.New("MariaDB backup contains no recognized checkpoint metadata")
}

func readBackupGTID(directory string) (string, error) {
	for _, name := range []string{"mariadb_backup_slave_info", "xtrabackup_slave_info"} {
		data, err := readMetadataFile(directory, name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("inspect MariaDB replica metadata: %w", err)
		}
		gtid := gtidPattern.FindString(string(data))
		if gtid == "" {
			return "", errors.New("MariaDB replica metadata contains no GTID position")
		}
		return gtid, nil
	}
	return "", errors.New("MariaDB backup contains no recognized replica metadata")
}

func readMetadataFile(directory, name string) ([]byte, error) {
	path := filepath.Join(directory, name)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("metadata path is not a regular file")
	}
	if info.Size() <= 0 || info.Size() > metadataFileLimit {
		return nil, errors.New("metadata file has an invalid size")
	}
	return os.ReadFile(path)
}
