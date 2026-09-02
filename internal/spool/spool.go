// Package spool separates durable capture from destination delivery.
package spool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/bionicman/savetoa/internal/agecrypto"
	"github.com/bionicman/savetoa/internal/localstore"
	"github.com/bionicman/savetoa/internal/manifest"
	"github.com/bionicman/savetoa/internal/s3store"
)

var ErrDestinationConflict = errors.New("destination contains a different backup set with the same ID")

type Spool struct {
	store *localstore.Store
}

func New(rootPath string) (*Spool, error) {
	store, err := localstore.New(rootPath)
	if err != nil {
		return nil, err
	}
	return &Spool{store: store}, nil
}

func (spool *Spool) Close() error {
	if spool == nil || spool.store == nil {
		return nil
	}
	return spool.store.Close()
}

func (spool *Spool) Stage(
	ctx context.Context,
	environment string,
	value manifest.Manifest,
	payload io.Reader,
) (*localstore.Set, error) {
	set, err := spool.store.Commit(ctx, environment, value, payload)
	if err != nil {
		return nil, fmt.Errorf("stage capture: %w", err)
	}
	return set, nil
}

func (spool *Spool) StageEncrypted(
	ctx context.Context,
	environment string,
	value manifest.Manifest,
	payload io.Reader,
	encryptor *agecrypto.Encryptor,
) (*localstore.Set, error) {
	if encryptor == nil {
		return nil, errors.New("age encryptor is required")
	}
	if value.Transformations == nil {
		return nil, errors.New("manifest transformations must be an array")
	}
	for _, transformation := range value.Transformations {
		if transformation.Driver == "age" {
			return nil, errors.New("manifest already contains age encryption")
		}
	}
	if strings.HasSuffix(value.Artifact.Filename, ".age") {
		return nil, errors.New("artifact filename already has an age suffix")
	}
	value.Artifact.Filename += ".age"
	value.Transformations = append(value.Transformations, encryptor.Transformation())
	encrypted, err := encryptor.EncryptReader(ctx, payload)
	if err != nil {
		return nil, err
	}
	return spool.Stage(ctx, environment, value, encrypted)
}

func (spool *Spool) Verify(ctx context.Context, relativePath string) (*localstore.Set, error) {
	set, err := spool.store.Verify(ctx, relativePath)
	if err != nil {
		return nil, fmt.Errorf("verify staged capture: %w", err)
	}
	return set, nil
}

func (spool *Spool) FindByID(backupID string) (*localstore.Set, error) {
	set, err := spool.store.FindByID(backupID)
	if err != nil {
		return nil, fmt.Errorf("find staged capture: %w", err)
	}
	return set, nil
}

func (spool *Spool) DeliverS3(
	ctx context.Context,
	relativePath string,
	destination *s3store.Store,
) (*localstore.Set, error) {
	if destination == nil {
		return nil, errors.New("S3 destination is required")
	}
	set, payload, err := spool.store.OpenVerifiedPayload(ctx, relativePath)
	if err != nil {
		return nil, fmt.Errorf("verify staged capture: %w", err)
	}
	defer payload.Close()
	metadataSet, manifestData, markerData, err := spool.store.ReadMetadata(relativePath)
	if err != nil {
		return nil, fmt.Errorf("read staged metadata: %w", err)
	}
	if !sameManifest(set.Manifest, metadataSet.Manifest) {
		return nil, errors.New("staged metadata changed during verification")
	}
	if err := destination.Deliver(ctx, relativePath, set, payload, manifestData, markerData); err != nil {
		return nil, fmt.Errorf("deliver staged capture to S3: %w", err)
	}
	return set, nil
}

func (spool *Spool) DeliverLocal(
	ctx context.Context,
	relativePath string,
	destination *localstore.Store,
) (*localstore.Set, error) {
	if destination == nil {
		return nil, errors.New("local destination is required")
	}
	source, payload, err := spool.store.OpenPayload(relativePath)
	if err != nil {
		return nil, fmt.Errorf("open staged capture: %w", err)
	}
	defer payload.Close()

	if existing, err := destination.Load(source.RelativePath); err == nil {
		if !sameManifest(existing.Manifest, source.Manifest) {
			return nil, ErrDestinationConflict
		}
		if _, err := spool.store.Verify(ctx, source.RelativePath); err != nil {
			return nil, fmt.Errorf("verify staged capture: %w", err)
		}
		verified, err := destination.Verify(ctx, existing.RelativePath)
		if err != nil {
			return nil, fmt.Errorf("verify existing destination set: %w", err)
		}
		return verified, nil
	}

	delivered, err := destination.Commit(ctx, source.Environment, source.Manifest, payload)
	if err != nil {
		return nil, fmt.Errorf("deliver staged capture: %w", err)
	}
	return delivered, nil
}

func sameManifest(left, right manifest.Manifest) bool {
	leftData, leftErr := manifest.Marshal(left)
	rightData, rightErr := manifest.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftData, rightData)
}
