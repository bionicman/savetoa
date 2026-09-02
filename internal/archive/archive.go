// Package archive streams safe tar and zstd representations of capture trees.
package archive

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/klauspost/compress/zstd"
)

func TarReader(ctx context.Context, rootPath string) (io.ReadCloser, error) {
	if ctx == nil {
		return nil, errors.New("context is required")
	}
	if err := validateRoot(rootPath); err != nil {
		return nil, err
	}
	reader, writer := io.Pipe()
	go func() {
		writer.CloseWithError(writeTar(ctx, writer, rootPath))
	}()
	return reader, nil
}

func TarZstdReader(ctx context.Context, rootPath string, level int) (io.ReadCloser, error) {
	if ctx == nil {
		return nil, errors.New("context is required")
	}
	if level < -5 || level > 22 {
		return nil, errors.New("zstd level must be between -5 and 22")
	}
	if err := validateRoot(rootPath); err != nil {
		return nil, err
	}
	reader, writer := io.Pipe()
	go func() {
		encoder, err := zstd.NewWriter(writer,
			zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(level)),
			zstd.WithEncoderConcurrency(1),
		)
		if err != nil {
			_ = writer.CloseWithError(errors.New("initialize zstd encoder"))
			return
		}
		err = writeTar(ctx, encoder, rootPath)
		closeErr := encoder.Close()
		if err == nil {
			err = closeErr
		}
		_ = writer.CloseWithError(err)
	}()
	return reader, nil
}

func validateRoot(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || path == string(filepath.Separator) {
		return errors.New("archive root must be a clean absolute path other than root")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect archive root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("archive root must be a real directory")
	}
	return nil
}

func writeTar(ctx context.Context, output io.Writer, rootPath string) error {
	entries := make([]string, 0)
	err := filepath.WalkDir(rootPath, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == rootPath {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("archive input contains symlink %q", entry.Name())
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("archive input contains unsupported file %q", entry.Name())
		}
		entries = append(entries, path)
		return nil
	})
	if err != nil {
		return fmt.Errorf("inspect archive input: %w", err)
	}
	sort.Strings(entries)

	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return fmt.Errorf("open archive root: %w", err)
	}
	defer root.Close()
	tape := tar.NewWriter(output)
	for _, path := range entries {
		if err := ctx.Err(); err != nil {
			_ = tape.Close()
			return err
		}
		relative, err := filepath.Rel(rootPath, path)
		if err != nil || relative == "." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			_ = tape.Close()
			return errors.New("archive input escaped its root")
		}
		info, err := os.Lstat(path)
		if err != nil {
			_ = tape.Close()
			return fmt.Errorf("inspect archive entry: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			_ = tape.Close()
			return fmt.Errorf("archive input changed to an unsupported file %q", filepath.Base(path))
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			_ = tape.Close()
			return fmt.Errorf("create archive header: %w", err)
		}
		header.Name = filepath.ToSlash(relative)
		if info.IsDir() {
			header.Name += "/"
		}
		if err := tape.WriteHeader(header); err != nil {
			_ = tape.Close()
			return fmt.Errorf("write archive header: %w", err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		file, err := root.Open(relative)
		if err != nil {
			_ = tape.Close()
			return fmt.Errorf("open archive entry: %w", err)
		}
		_, copyErr := copyContext(ctx, tape, file)
		closeErr := file.Close()
		if copyErr != nil {
			_ = tape.Close()
			return fmt.Errorf("write archive entry: %w", copyErr)
		}
		if closeErr != nil {
			_ = tape.Close()
			return fmt.Errorf("close archive entry: %w", closeErr)
		}
	}
	if err := tape.Close(); err != nil {
		return fmt.Errorf("finish tar archive: %w", err)
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
