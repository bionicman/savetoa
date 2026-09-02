// Package restore verifies and safely materializes completed backup sets.
package restore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"filippo.io/age"
	"github.com/bionicman/savetoa/internal/agecrypto"
	archivepkg "github.com/bionicman/savetoa/internal/archive"
	"github.com/bionicman/savetoa/internal/localstore"
	"github.com/bionicman/savetoa/internal/manifest"
	"github.com/klauspost/compress/zstd"
)

type Options struct {
	SourceRoot   string
	BackupID     string
	TargetDir    string
	IdentityFile string
}

func Materialize(ctx context.Context, options Options) (*localstore.Set, error) {
	if ctx == nil {
		return nil, errors.New("context is required")
	}
	store, err := localstore.New(options.SourceRoot)
	if err != nil {
		return nil, fmt.Errorf("open restore source: %w", err)
	}
	defer store.Close()
	set, err := store.FindByID(options.BackupID)
	if err != nil {
		return nil, fmt.Errorf("find backup: %w", err)
	}
	if err := validateArtifactShape(set.Manifest); err != nil {
		return nil, err
	}

	_, payload, err := store.OpenVerifiedPayload(ctx, set.RelativePath)
	if err != nil {
		return nil, fmt.Errorf("verify backup: %w", err)
	}
	defer payload.Close()
	var reader io.Reader = payload
	var decoder *zstd.Decoder
	for index := len(set.Manifest.Transformations) - 1; index >= 0; index-- {
		switch set.Manifest.Transformations[index].Driver {
		case "age":
			identities, err := readIdentities(options.IdentityFile)
			if err != nil {
				return nil, err
			}
			reader, err = agecrypto.DecryptReader(ctx, reader, identities)
			if err != nil {
				return nil, err
			}
		case "zstd":
			decoder, err = zstd.NewReader(reader, zstd.WithDecoderConcurrency(1))
			if err != nil {
				return nil, errors.New("initialize zstd decompression")
			}
			defer decoder.Close()
			reader = decoder
		default:
			return nil, errors.New("backup uses an unsupported transformation")
		}
	}
	if options.IdentityFile != "" && !hasTransformation(set.Manifest.Transformations, "age") {
		return nil, errors.New("identity file was provided for an unencrypted backup")
	}
	if err := archivepkg.ExtractTar(ctx, reader, options.TargetDir); err != nil {
		return nil, fmt.Errorf("materialize backup: %w", err)
	}
	return set, nil
}

func validateArtifactShape(value manifest.Manifest) error {
	filename := "payload.tar"
	for _, transformation := range value.Transformations {
		switch transformation.Driver {
		case "zstd":
			filename += ".zst"
		case "age":
			filename += ".age"
		default:
			return errors.New("backup uses an unsupported transformation")
		}
	}
	if value.Artifact.Filename != filename {
		return errors.New("artifact filename does not match its transformation chain")
	}
	return nil
}

func readIdentities(path string) ([]age.Identity, error) {
	if path == "" {
		return nil, errors.New("encrypted backup requires --identity-file")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, errors.New("inspect age identity file")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, errors.New("age identity file must be a regular non-symlink file with mode 0600")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("open age identity file")
	}
	identities, parseErr := agecrypto.ParseIdentities(file)
	closeErr := file.Close()
	if parseErr != nil {
		return nil, parseErr
	}
	if closeErr != nil {
		return nil, errors.New("close age identity file")
	}
	return identities, nil
}

func hasTransformation(transformations []manifest.Transformation, driver string) bool {
	for _, transformation := range transformations {
		if transformation.Driver == driver {
			return true
		}
	}
	return false
}
