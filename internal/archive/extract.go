package archive

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	maxArchiveEntries = 1_000_000
	maxArchivePath    = 4096
)

type entryMetadata struct {
	mode    os.FileMode
	uid     int
	gid     int
	atime   time.Time
	modTime time.Time
}

// ExtractTar materializes a trusted-by-checksum but structurally untrusted tar
// stream into a newly created explicit destination.
func ExtractTar(ctx context.Context, input io.Reader, targetPath string) error {
	if ctx == nil {
		return errors.New("context is required")
	}
	if input == nil {
		return errors.New("archive reader is required")
	}
	if targetPath == "" || !filepath.IsAbs(targetPath) || filepath.Clean(targetPath) != targetPath || targetPath == string(filepath.Separator) {
		return errors.New("restore target must be a clean absolute path other than root")
	}
	parentPath, base := filepath.Dir(targetPath), filepath.Base(targetPath)
	parentInfo, err := os.Lstat(parentPath)
	if err != nil {
		return fmt.Errorf("inspect restore parent: %w", err)
	}
	if parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() {
		return errors.New("restore parent must be a real directory")
	}
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		return fmt.Errorf("open restore parent: %w", err)
	}
	defer parent.Close()
	if err := parent.Mkdir(base, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return errors.New("restore target already exists")
		}
		return fmt.Errorf("create restore target: %w", err)
	}
	complete := false
	defer func() {
		if !complete {
			_ = parent.RemoveAll(base)
		}
	}()
	root, err := os.OpenRoot(targetPath)
	if err != nil {
		return fmt.Errorf("open restore target: %w", err)
	}
	defer root.Close()

	tape := tar.NewReader(input)
	seen := make(map[string]struct{})
	directories := map[string]entryMetadata{".": {mode: 0o700}}
	for count := 0; ; count++ {
		if count == maxArchiveEntries {
			return fmt.Errorf("archive contains more than %d entries", maxArchiveEntries)
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("extract archive: %w", err)
		}
		header, err := tape.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar header: %w", err)
		}
		name, err := safeArchivePath(header.Name)
		if err != nil {
			return err
		}
		if _, exists := seen[name]; exists {
			return fmt.Errorf("archive contains duplicate path %q", name)
		}
		seen[name] = struct{}{}
		if err := ensureExtractParents(root, path.Dir(name), directories); err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := root.Mkdir(name, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return fmt.Errorf("create archive directory %q: %w", name, err)
			}
			info, err := root.Lstat(name)
			if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("archive directory %q conflicts with another entry", name)
			}
			directories[name] = entryMetadata{
				mode: os.FileMode(header.Mode) & 0o777, uid: header.Uid, gid: header.Gid,
				atime: header.AccessTime, modTime: header.ModTime,
			}
		case tar.TypeReg, tar.TypeRegA:
			file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return fmt.Errorf("create archive file %q: %w", name, err)
			}
			_, copyErr := copyContext(ctx, file, io.LimitReader(tape, header.Size))
			syncErr := file.Sync()
			closeErr := file.Close()
			if copyErr != nil {
				return fmt.Errorf("extract archive file %q: %w", name, copyErr)
			}
			if syncErr != nil {
				return fmt.Errorf("persist archive file %q: %w", name, syncErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close archive file %q: %w", name, closeErr)
			}
			if os.Geteuid() == 0 {
				if err := root.Chown(name, header.Uid, header.Gid); err != nil {
					return fmt.Errorf("set archive file ownership %q: %w", name, err)
				}
			}
			if err := root.Chmod(name, os.FileMode(header.Mode)&0o777); err != nil {
				return fmt.Errorf("set archive file mode %q: %w", name, err)
			}
			if !header.ModTime.IsZero() {
				atime := header.AccessTime
				if atime.IsZero() {
					atime = header.ModTime
				}
				if err := root.Chtimes(name, atime, header.ModTime); err != nil {
					return fmt.Errorf("set archive file time %q: %w", name, err)
				}
			}
		case tar.TypeSymlink:
			if err := validateArchiveLink(name, header.Linkname); err != nil {
				return err
			}
			if err := root.Symlink(header.Linkname, name); err != nil {
				return fmt.Errorf("create archive symlink %q: %w", name, err)
			}
			if os.Geteuid() == 0 {
				if err := root.Lchown(name, header.Uid, header.Gid); err != nil {
					return fmt.Errorf("set archive symlink ownership %q: %w", name, err)
				}
			}
		default:
			return fmt.Errorf("archive path %q has unsupported type", name)
		}
	}

	directoryNames := make([]string, 0, len(directories))
	for name := range directories {
		directoryNames = append(directoryNames, name)
	}
	sort.Slice(directoryNames, func(i, j int) bool {
		return strings.Count(directoryNames[i], "/") > strings.Count(directoryNames[j], "/")
	})
	for _, name := range directoryNames {
		metadata := directories[name]
		if name != "." {
			if os.Geteuid() == 0 {
				if err := root.Chown(name, metadata.uid, metadata.gid); err != nil {
					return fmt.Errorf("set archive directory ownership %q: %w", name, err)
				}
			}
			if err := root.Chmod(name, metadata.mode); err != nil {
				return fmt.Errorf("set archive directory mode %q: %w", name, err)
			}
			if !metadata.modTime.IsZero() {
				atime := metadata.atime
				if atime.IsZero() {
					atime = metadata.modTime
				}
				if err := root.Chtimes(name, atime, metadata.modTime); err != nil {
					return fmt.Errorf("set archive directory time %q: %w", name, err)
				}
			}
		}
		directory, err := root.Open(name)
		if err != nil {
			return fmt.Errorf("open archive directory %q: %w", name, err)
		}
		syncErr := directory.Sync()
		closeErr := directory.Close()
		if syncErr != nil {
			return fmt.Errorf("persist archive directory %q: %w", name, syncErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close archive directory %q: %w", name, closeErr)
		}
	}
	if err := parent.Chmod(base, directories["."].mode); err != nil {
		return fmt.Errorf("set restore target mode: %w", err)
	}
	parentDirectory, err := parent.Open(".")
	if err != nil {
		return fmt.Errorf("open restore parent for persistence: %w", err)
	}
	syncErr := parentDirectory.Sync()
	closeErr := parentDirectory.Close()
	if syncErr != nil {
		return fmt.Errorf("persist restore target: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close restore parent: %w", closeErr)
	}
	complete = true
	return nil
}

func safeArchivePath(name string) (string, error) {
	if name == "" || len(name) > maxArchivePath || strings.Contains(name, "\\") || strings.HasPrefix(name, "/") {
		return "", errors.New("archive contains an unsafe path")
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != strings.TrimSuffix(name, "/") {
		return "", errors.New("archive contains an unsafe path")
	}
	return filepath.FromSlash(clean), nil
}

func validateArchiveLink(name, target string) error {
	if target == "" || len(target) > maxArchivePath || strings.Contains(target, "\\") || path.IsAbs(target) {
		return fmt.Errorf("archive symlink %q has an unsafe target", name)
	}
	resolved := path.Clean(path.Join(path.Dir(filepath.ToSlash(name)), target))
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return fmt.Errorf("archive symlink %q escapes the restore target", name)
	}
	return nil
}

func ensureExtractParents(root *os.Root, directory string, directories map[string]entryMetadata) error {
	if directory == "." {
		return nil
	}
	current := ""
	for _, component := range strings.Split(filepath.FromSlash(directory), string(filepath.Separator)) {
		if current == "" {
			current = component
		} else {
			current = filepath.Join(current, component)
		}
		if err := root.Mkdir(current, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create archive parent %q: %w", current, err)
		}
		info, err := root.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("archive parent %q is not a real directory", current)
		}
		if _, exists := directories[current]; !exists {
			directories[current] = entryMetadata{mode: 0o700}
		}
	}
	return nil
}
