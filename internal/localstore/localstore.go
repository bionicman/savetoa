// Package localstore writes and verifies durable backup sets below one root.
package localstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bionicman/savetoa/internal/manifest"
	"golang.org/x/sys/unix"
)

const (
	ManifestFilename = "manifest.json"
	CompleteFilename = "complete"
	maxMarkerSize    = 128
)

const (
	manifestFilename = ManifestFilename
	completeFilename = CompleteFilename
)

var (
	ErrSetExists        = errors.New("backup set already exists")
	ErrSetNotFound      = errors.New("backup set not found")
	ErrAmbiguousSet     = errors.New("backup ID is not unique")
	ErrArtifactMismatch = errors.New("payload does not match expected artifact metadata")
	componentPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	backupIDPattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	yearPattern         = regexp.MustCompile(`^[0-9]{4}$`)
	datePartPattern     = regexp.MustCompile(`^[0-9]{2}$`)
)

func (store *Store) FindByID(backupID string) (*Set, error) {
	if store == nil || store.root == nil {
		return nil, errors.New("local destination is closed")
	}
	if !backupIDPattern.MatchString(backupID) {
		return nil, errors.New("backup ID is invalid")
	}
	var matches []string
	err := fs.WalkDir(store.root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if !entry.IsDir() || path == "." {
			return nil
		}
		depth := strings.Count(filepath.ToSlash(path), "/") + 1
		if depth == 6 {
			if filepath.Base(path) == backupID {
				marker, err := store.root.Lstat(filepath.Join(path, completeFilename))
				if errors.Is(err, os.ErrNotExist) {
					return fs.SkipDir
				}
				if err != nil {
					return err
				}
				if marker.Mode().IsRegular() && marker.Mode()&os.ModeSymlink == 0 {
					matches = append(matches, path)
				} else {
					return errors.New("backup completion marker is not a regular file")
				}
			}
			return fs.SkipDir
		}
		if depth > 6 {
			return fs.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan local destination: %w", err)
	}
	if len(matches) == 0 {
		return nil, ErrSetNotFound
	}
	if len(matches) != 1 {
		return nil, ErrAmbiguousSet
	}
	return store.Load(matches[0])
}

type Store struct {
	root *os.Root
}

type Set struct {
	Environment  string
	RelativePath string
	Manifest     manifest.Manifest
}

func New(rootPath string) (*Store, error) {
	if rootPath == "" || !filepath.IsAbs(rootPath) || filepath.Clean(rootPath) != rootPath {
		return nil, errors.New("local destination must be a clean absolute path")
	}
	if rootPath == string(filepath.Separator) {
		return nil, errors.New("local destination must not be the filesystem root")
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, fmt.Errorf("open local destination: %w", err)
	}
	return &Store{root: root}, nil
}

func (store *Store) Close() error {
	if store == nil || store.root == nil {
		return nil
	}
	return store.root.Close()
}

func (store *Store) Commit(
	ctx context.Context,
	environment string,
	value manifest.Manifest,
	payload io.Reader,
) (*Set, error) {
	if ctx == nil {
		return nil, errors.New("context is required")
	}
	if payload == nil {
		return nil, errors.New("payload reader is required")
	}
	if err := validateComponent("environment", environment); err != nil {
		return nil, err
	}
	if err := validateComponent("target", value.Target); err != nil {
		return nil, err
	}
	if !backupIDPattern.MatchString(value.BackupID) {
		return nil, errors.New("backup ID is invalid")
	}

	hasExpectedArtifact := value.Artifact.SizeBytes != 0 || value.Artifact.Checksum.Algorithm != "" || value.Artifact.Checksum.Value != ""
	preflight := value
	if !hasExpectedArtifact {
		preflight.Artifact.SizeBytes = 1
		preflight.Artifact.Checksum = manifest.Checksum{
			Algorithm: "sha256",
			Value:     strings.Repeat("0", sha256.Size*2),
		}
	}
	if _, err := manifest.Marshal(preflight); err != nil {
		return nil, fmt.Errorf("validate manifest metadata: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("commit backup set: %w", err)
	}

	started := value.StartedAt.UTC()
	parent := filepath.Join(
		environment,
		value.Target,
		fmt.Sprintf("%04d", started.Year()),
		fmt.Sprintf("%02d", int(started.Month())),
		fmt.Sprintf("%02d", started.Day()),
	)
	if err := store.ensureHierarchy(parent); err != nil {
		return nil, err
	}

	suffix, err := randomSuffix()
	if err != nil {
		return nil, err
	}
	partialBase := "." + value.BackupID + ".partial-" + suffix
	partialPath := filepath.Join(parent, partialBase)
	if err := store.root.Mkdir(partialPath, 0o700); err != nil {
		return nil, fmt.Errorf("create partial backup set: %w", err)
	}
	if err := store.syncDirectory(parent); err != nil {
		return nil, fmt.Errorf("persist partial backup set directory: %w", err)
	}
	published := false
	defer func() {
		if !published {
			store.cleanupPartial(parent, partialPath, value.Artifact.Filename)
		}
	}()

	size, payloadDigest, err := store.writeExclusive(ctx, partialPath, value.Artifact.Filename, payload)
	if err != nil {
		return nil, fmt.Errorf("write payload: %w", err)
	}
	if size == 0 {
		return nil, errors.New("write payload: payload is empty")
	}
	computedChecksum := manifest.Checksum{
		Algorithm: "sha256",
		Value:     hex.EncodeToString(payloadDigest),
	}
	if hasExpectedArtifact {
		if value.Artifact.SizeBytes != size || value.Artifact.Checksum != computedChecksum {
			return nil, ErrArtifactMismatch
		}
	} else {
		value.Artifact.SizeBytes = size
		value.Artifact.Checksum = computedChecksum
	}

	manifestData, err := manifest.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode final manifest: %w", err)
	}
	_, manifestDigest, err := store.writeExclusive(ctx, partialPath, manifestFilename, bytes.NewReader(manifestData))
	if err != nil {
		return nil, fmt.Errorf("write manifest: %w", err)
	}

	marker := "sha256:" + hex.EncodeToString(manifestDigest) + "\n"
	if _, _, err := store.writeExclusive(ctx, partialPath, completeFilename, strings.NewReader(marker)); err != nil {
		return nil, fmt.Errorf("write completion marker: %w", err)
	}
	if err := store.publishDirectory(ctx, parent, partialBase, value.BackupID); err != nil {
		return nil, err
	}
	published = true

	setPath := filepath.Join(parent, value.BackupID)
	return &Set{Environment: environment, RelativePath: setPath, Manifest: value}, nil
}

func (store *Store) Load(relativePath string) (*Set, error) {
	if err := ValidateSetPath(relativePath); err != nil {
		return nil, err
	}

	markerData, err := store.readRegularFile(filepath.Join(relativePath, completeFilename), maxMarkerSize)
	if err != nil {
		return nil, fmt.Errorf("read completion marker: %w", err)
	}
	manifestData, err := store.readRegularFile(filepath.Join(relativePath, manifestFilename), manifest.MaxFileSize)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	return ValidateMetadata(relativePath, manifestData, markerData)
}

// ValidateSetPath rejects paths that do not match the backup-set layout.
func ValidateSetPath(relativePath string) error {
	_, err := validateSetPath(relativePath)
	return err
}

// ValidateMetadata treats manifest and marker bytes as untrusted input and
// binds them to an already validated backup-set path.
func ValidateMetadata(relativePath string, manifestData, markerData []byte) (*Set, error) {
	parts, err := validateSetPath(relativePath)
	if err != nil {
		return nil, err
	}
	if len(manifestData) > manifest.MaxFileSize {
		return nil, fmt.Errorf("manifest exceeds %d bytes", manifest.MaxFileSize)
	}
	if len(markerData) > maxMarkerSize {
		return nil, fmt.Errorf("completion marker exceeds %d bytes", maxMarkerSize)
	}
	expectedManifestDigest, err := parseMarker(markerData)
	if err != nil {
		return nil, err
	}
	actualManifestDigest := sha256.Sum256(manifestData)
	if subtle.ConstantTimeCompare(expectedManifestDigest, actualManifestDigest[:]) != 1 {
		return nil, errors.New("completion marker does not match manifest")
	}
	value, err := manifest.Parse(manifestData)
	if err != nil {
		return nil, err
	}
	if value.Target != parts[1] || value.BackupID != parts[5] {
		return nil, errors.New("manifest identity does not match backup set path")
	}
	started := value.StartedAt.UTC()
	if parts[2] != fmt.Sprintf("%04d", started.Year()) ||
		parts[3] != fmt.Sprintf("%02d", int(started.Month())) ||
		parts[4] != fmt.Sprintf("%02d", started.Day()) {
		return nil, errors.New("manifest start date does not match backup set path")
	}
	return &Set{Environment: parts[0], RelativePath: relativePath, Manifest: *value}, nil
}

func (store *Store) OpenPayload(relativePath string) (*Set, *os.File, error) {
	set, err := store.Load(relativePath)
	if err != nil {
		return nil, nil, err
	}
	payloadPath := filepath.Join(relativePath, set.Manifest.Artifact.Filename)
	file, err := store.openRegularFile(payloadPath)
	if err != nil {
		return nil, nil, fmt.Errorf("open payload: %w", err)
	}
	return set, file, nil
}

func (store *Store) Verify(ctx context.Context, relativePath string) (*Set, error) {
	set, file, err := store.OpenVerifiedPayload(ctx, relativePath)
	if err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close payload: %w", err)
	}
	return set, nil
}

// ReadMetadata returns the exact durable manifest and completion-marker bytes.
// It validates their relationship before returning either file.
func (store *Store) ReadMetadata(relativePath string) (*Set, []byte, []byte, error) {
	set, err := store.Load(relativePath)
	if err != nil {
		return nil, nil, nil, err
	}
	manifestData, err := store.readRegularFile(filepath.Join(relativePath, manifestFilename), manifest.MaxFileSize)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read manifest: %w", err)
	}
	markerData, err := store.readRegularFile(filepath.Join(relativePath, completeFilename), maxMarkerSize)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read completion marker: %w", err)
	}
	expected, err := parseMarker(markerData)
	if err != nil {
		return nil, nil, nil, err
	}
	actual := sha256.Sum256(manifestData)
	if subtle.ConstantTimeCompare(expected, actual[:]) != 1 {
		return nil, nil, nil, errors.New("completion marker does not match manifest")
	}
	return set, manifestData, markerData, nil
}

// OpenVerifiedPayload returns the same open payload file descriptor that was
// hashed, rewound to its beginning.
func (store *Store) OpenVerifiedPayload(ctx context.Context, relativePath string) (*Set, *os.File, error) {
	if ctx == nil {
		return nil, nil, errors.New("context is required")
	}
	set, file, err := store.OpenPayload(relativePath)
	if err != nil {
		return nil, nil, err
	}
	hasher := sha256.New()
	size, copyErr := copyWithContext(ctx, hasher, file)
	if copyErr != nil {
		_ = file.Close()
		return nil, nil, fmt.Errorf("verify payload: %w", copyErr)
	}
	if size != set.Manifest.Artifact.SizeBytes {
		_ = file.Close()
		return nil, nil, errors.New("payload size does not match manifest")
	}
	expected, err := hex.DecodeString(set.Manifest.Artifact.Checksum.Value)
	if err != nil {
		_ = file.Close()
		return nil, nil, errors.New("manifest contains an invalid payload checksum")
	}
	if subtle.ConstantTimeCompare(expected, hasher.Sum(nil)) != 1 {
		_ = file.Close()
		return nil, nil, errors.New("payload checksum does not match manifest")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return nil, nil, fmt.Errorf("rewind verified payload: %w", err)
	}
	return set, file, nil
}

func (store *Store) ensureHierarchy(relativePath string) error {
	current := "."
	for _, component := range strings.Split(relativePath, string(filepath.Separator)) {
		parent := current
		current = filepath.Join(current, component)
		err := store.root.Mkdir(current, 0o700)
		if err == nil {
			if err := store.syncDirectory(parent); err != nil {
				return fmt.Errorf("persist directory %q: %w", current, err)
			}
			continue
		}
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create directory %q: %w", current, err)
		}
		info, err := store.root.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspect directory %q: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("path component %q is not a real directory", current)
		}
	}
	return nil
}

func (store *Store) writeExclusive(
	ctx context.Context,
	directory string,
	filename string,
	reader io.Reader,
) (int64, []byte, error) {
	finalName := filepath.Join(directory, filename)
	file, err := store.root.OpenFile(finalName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, nil, err
	}
	removeFile := true
	defer func() {
		if removeFile {
			_ = store.root.Remove(finalName)
			_ = store.syncDirectory(directory)
		}
	}()

	hasher := sha256.New()
	size, copyErr := copyWithContext(ctx, io.MultiWriter(file, hasher), reader)
	if copyErr != nil {
		_ = file.Close()
		return 0, nil, copyErr
	}
	if err := ctx.Err(); err != nil {
		_ = file.Close()
		return 0, nil, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return 0, nil, err
	}
	if err := file.Close(); err != nil {
		return 0, nil, err
	}
	removeFile = false
	if err := store.syncDirectory(directory); err != nil {
		return 0, nil, err
	}
	return size, hasher.Sum(nil), nil
}

func (store *Store) syncDirectory(relativePath string) error {
	directory, err := store.root.Open(relativePath)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func (store *Store) cleanupPartial(parent, partialPath, artifactFilename string) {
	for _, filename := range []string{completeFilename, manifestFilename, artifactFilename} {
		_ = store.root.Remove(filepath.Join(partialPath, filename))
	}
	_ = store.root.Remove(partialPath)
	_ = store.syncDirectory(parent)
}

func (store *Store) publishDirectory(ctx context.Context, parent, partialBase, finalBase string) error {
	directory, err := store.root.Open(parent)
	if err != nil {
		return fmt.Errorf("open publication directory: %w", err)
	}
	defer directory.Close()
	if err := flockContext(ctx, directory); err != nil {
		return fmt.Errorf("lock publication directory: %w", err)
	}
	defer func() { _ = unix.Flock(int(directory.Fd()), unix.LOCK_UN) }()

	finalPath := filepath.Join(parent, finalBase)
	if _, err := store.root.Lstat(finalPath); err == nil {
		return fmt.Errorf("%w: %s", ErrSetExists, finalPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect final backup set: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("publish backup set: %w", err)
	}
	fd := int(directory.Fd())
	if err := unix.Renameat(fd, partialBase, fd, finalBase); err != nil {
		return fmt.Errorf("publish backup set: %w", err)
	}
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("persist published backup set: %w", err)
	}
	return nil
}

func (store *Store) readRegularFile(relativePath string, limit int64) ([]byte, error) {
	file, err := store.openRegularFile(relativePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	return data, nil
}

func (store *Store) openRegularFile(relativePath string) (*os.File, error) {
	info, err := store.root.Lstat(relativePath)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("path is not a regular file")
	}
	return store.root.Open(relativePath)
}

func validateSetPath(relativePath string) ([]string, error) {
	if relativePath == "" || filepath.IsAbs(relativePath) || filepath.Clean(relativePath) != relativePath {
		return nil, errors.New("backup set path must be clean and relative")
	}
	parts := strings.Split(relativePath, string(filepath.Separator))
	if len(parts) != 6 {
		return nil, errors.New("backup set path must have environment, target, date, and ID components")
	}
	if err := validateComponent("environment", parts[0]); err != nil {
		return nil, err
	}
	if err := validateComponent("target", parts[1]); err != nil {
		return nil, err
	}
	if !yearPattern.MatchString(parts[2]) || !datePartPattern.MatchString(parts[3]) || !datePartPattern.MatchString(parts[4]) {
		return nil, errors.New("backup set path contains an invalid date")
	}
	year, _ := strconv.Atoi(parts[2])
	month, _ := strconv.Atoi(parts[3])
	day, _ := strconv.Atoi(parts[4])
	date := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
	if date.Year() != year || int(date.Month()) != month || date.Day() != day {
		return nil, errors.New("backup set path contains an invalid date")
	}
	if !backupIDPattern.MatchString(parts[5]) {
		return nil, errors.New("backup set path contains an invalid backup ID")
	}
	return parts, nil
}

func validateComponent(kind, value string) error {
	if !componentPattern.MatchString(value) {
		return fmt.Errorf("%s %q must match %s", kind, value, componentPattern.String())
	}
	return nil
}

func parseMarker(data []byte) ([]byte, error) {
	marker := string(data)
	if len(marker) != len("sha256:")+sha256.Size*2+1 || !strings.HasPrefix(marker, "sha256:") || !strings.HasSuffix(marker, "\n") {
		return nil, errors.New("completion marker is invalid")
	}
	digest, err := hex.DecodeString(strings.TrimSuffix(strings.TrimPrefix(marker, "sha256:"), "\n"))
	encodedDigest := strings.TrimSuffix(strings.TrimPrefix(marker, "sha256:"), "\n")
	if err != nil || len(digest) != sha256.Size || strings.ToLower(encodedDigest) != encodedDigest {
		return nil, errors.New("completion marker is invalid")
	}
	return digest, nil
}

func randomSuffix() (string, error) {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate partial directory name: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func flockContext(ctx context.Context, file *os.File) error {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func copyWithContext(ctx context.Context, destination io.Writer, source io.Reader) (int64, error) {
	return io.Copy(destination, &contextReader{ctx: ctx, reader: source})
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *contextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}
