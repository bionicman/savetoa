// Package s3store publishes immutable backup sets to S3-compatible storage.
package s3store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/bionicman/savetoa/internal/localstore"
	"github.com/bionicman/savetoa/internal/manifest"
	"go.yaml.in/yaml/v3"
)

const (
	maxCredentialsSize = 64 << 10
	MaxObjectSize      = int64(5 << 30)
)

var ErrConflict = errors.New("destination contains different object data at the backup key")

type Credentials struct {
	AccessKeyID     string `yaml:"access_key_id"`
	SecretAccessKey string `yaml:"secret_access_key"`
	SessionToken    string `yaml:"session_token,omitempty"`
}

type Options struct {
	Endpoint    string
	Region      string
	Bucket      string
	Prefix      string
	Credentials Credentials
	HTTPClient  *http.Client
}

type objectAPI interface {
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

type Store struct {
	client objectAPI
	bucket string
	prefix string
}

type Object struct {
	Key  string
	Body io.ReadSeeker
	Size int64
}

func ReadCredentialsFile(filename string) (Credentials, error) {
	info, err := os.Lstat(filename)
	if err != nil {
		return Credentials{}, errors.New("inspect S3 credentials file")
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return Credentials{}, errors.New("S3 credentials path must be a regular file, not a symlink")
	}
	if info.Mode().Perm() != 0o600 {
		return Credentials{}, errors.New("S3 credentials file permissions must be 0600")
	}
	file, err := os.Open(filename)
	if err != nil {
		return Credentials{}, errors.New("open S3 credentials file")
	}
	defer file.Close()
	return ReadCredentials(file)
}

func ReadCredentials(reader io.Reader) (Credentials, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxCredentialsSize+1))
	if err != nil {
		return Credentials{}, errors.New("read S3 credentials")
	}
	if len(data) > maxCredentialsSize {
		return Credentials{}, errors.New("S3 credentials file is too large")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var result Credentials
	if err := decoder.Decode(&result); err != nil {
		return Credentials{}, errors.New("decode S3 credentials")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Credentials{}, errors.New("S3 credentials must contain exactly one YAML document")
	}
	if strings.TrimSpace(result.AccessKeyID) == "" || strings.TrimSpace(result.SecretAccessKey) == "" {
		return Credentials{}, errors.New("S3 access_key_id and secret_access_key are required")
	}
	for _, value := range []string{result.AccessKeyID, result.SecretAccessKey, result.SessionToken} {
		if strings.ContainsAny(value, "\r\n\x00") {
			return Credentials{}, errors.New("S3 credentials must be single-line values")
		}
	}
	return result, nil
}

func New(options Options) (*Store, error) {
	if options.Endpoint == "" || options.Region == "" || options.Bucket == "" {
		return nil, errors.New("S3 endpoint, region, and bucket are required")
	}
	if strings.TrimSpace(options.Credentials.AccessKeyID) == "" || strings.TrimSpace(options.Credentials.SecretAccessKey) == "" {
		return nil, errors.New("S3 credentials are required")
	}
	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: 30 * time.Minute,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return errors.New("S3 redirects are disabled")
			},
		}
	}
	awsConfig := aws.Config{
		Region:                     options.Region,
		Credentials:                aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(options.Credentials.AccessKeyID, options.Credentials.SecretAccessKey, options.Credentials.SessionToken)),
		HTTPClient:                 httpClient,
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}
	client := s3.NewFromConfig(awsConfig, func(value *s3.Options) {
		value.BaseEndpoint = aws.String(options.Endpoint)
		value.UsePathStyle = true
	})
	return &Store{client: client, bucket: options.Bucket, prefix: options.Prefix}, nil
}

func (store *Store) Deliver(ctx context.Context, relativePath string, set *localstore.Set, payload io.ReadSeeker, manifestData, markerData []byte) error {
	if ctx == nil {
		return errors.New("context is required")
	}
	if store == nil || store.client == nil || set == nil || payload == nil {
		return errors.New("S3 destination, backup set, and payload are required")
	}
	if set.RelativePath != relativePath {
		return errors.New("backup set path does not match delivery path")
	}
	if set.Manifest.Artifact.SizeBytes > MaxObjectSize {
		return fmt.Errorf("payload exceeds the %d-byte single-object delivery limit", MaxObjectSize)
	}
	objects := []Object{
		{Key: store.key(relativePath, set.Manifest.Artifact.Filename), Body: payload, Size: set.Manifest.Artifact.SizeBytes},
		{Key: store.key(relativePath, localstore.ManifestFilename), Body: bytes.NewReader(manifestData), Size: int64(len(manifestData))},
		{Key: store.key(relativePath, localstore.CompleteFilename), Body: bytes.NewReader(markerData), Size: int64(len(markerData))},
	}
	for _, object := range objects {
		if err := store.publish(ctx, object); err != nil {
			return err
		}
	}
	return nil
}

// Fetch verifies one completed remote set and atomically imports it into a
// local store. Remote data is never exposed as a completed local set until the
// completion marker, manifest identity, payload size, and checksum all match.
func (store *Store) Fetch(ctx context.Context, relativePath string, destination *localstore.Store) (*localstore.Set, error) {
	if ctx == nil {
		return nil, errors.New("context is required")
	}
	if store == nil || store.client == nil || destination == nil {
		return nil, errors.New("S3 source and local destination are required")
	}
	if err := localstore.ValidateSetPath(relativePath); err != nil {
		return nil, err
	}

	markerData, err := store.readObject(ctx, store.key(relativePath, localstore.CompleteFilename), 128)
	if err != nil {
		return nil, errors.New("read S3 completion marker")
	}
	manifestData, err := store.readObject(ctx, store.key(relativePath, localstore.ManifestFilename), manifest.MaxFileSize)
	if err != nil {
		return nil, errors.New("read S3 manifest")
	}
	remote, err := localstore.ValidateMetadata(relativePath, manifestData, markerData)
	if err != nil {
		return nil, fmt.Errorf("validate S3 metadata: %w", err)
	}
	canonicalManifest, err := manifest.Marshal(remote.Manifest)
	if err != nil || !bytes.Equal(canonicalManifest, manifestData) {
		return nil, errors.New("validate S3 metadata: manifest is not canonical")
	}
	if remote.Manifest.Artifact.SizeBytes > MaxObjectSize {
		return nil, fmt.Errorf("S3 payload exceeds the %d-byte single-object recovery limit", MaxObjectSize)
	}

	if existing, loadErr := destination.Load(relativePath); loadErr == nil {
		if !sameManifest(existing.Manifest, remote.Manifest) {
			return nil, ErrConflict
		}
		return destination.Verify(ctx, relativePath)
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		// Load wraps filesystem absence; any published path that cannot be
		// validated must not be replaced by a fetch.
		return nil, fmt.Errorf("inspect local fetch destination: %w", loadErr)
	}

	payload, err := store.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(store.bucket),
		Key:    aws.String(store.key(relativePath, remote.Manifest.Artifact.Filename)),
	})
	if err != nil {
		return nil, errors.New("read S3 payload")
	}
	defer payload.Body.Close()
	if payload.ContentLength != nil && *payload.ContentLength != remote.Manifest.Artifact.SizeBytes {
		return nil, errors.New("S3 payload size does not match manifest")
	}
	limited := io.LimitReader(payload.Body, remote.Manifest.Artifact.SizeBytes+1)
	imported, err := destination.Commit(ctx, remote.Environment, remote.Manifest, limited)
	if errors.Is(err, localstore.ErrSetExists) {
		existing, verifyErr := destination.Verify(ctx, relativePath)
		if verifyErr != nil {
			return nil, fmt.Errorf("verify concurrently imported set: %w", verifyErr)
		}
		if !sameManifest(existing.Manifest, remote.Manifest) {
			return nil, ErrConflict
		}
		return existing, nil
	}
	if err != nil {
		return nil, fmt.Errorf("import S3 backup set: %w", err)
	}
	return imported, nil
}

func (store *Store) readObject(ctx context.Context, key string, limit int64) ([]byte, error) {
	result, err := store.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(store.bucket), Key: aws.String(key)})
	if err != nil {
		return nil, err
	}
	defer result.Body.Close()
	if result.ContentLength != nil && *result.ContentLength > limit {
		return nil, errors.New("S3 object exceeds size limit")
	}
	data, err := io.ReadAll(io.LimitReader(result.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("S3 object exceeds size limit")
	}
	return data, nil
}

func sameManifest(left, right manifest.Manifest) bool {
	leftData, leftErr := manifest.Marshal(left)
	rightData, rightErr := manifest.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftData, rightData)
}

func (store *Store) key(relativePath, filename string) string {
	return path.Join(store.prefix, strings.ReplaceAll(relativePath, `\\`, "/"), filename)
}

func (store *Store) SetPrefix(relativePath string) string {
	return path.Join(store.prefix, strings.ReplaceAll(relativePath, `\\`, "/"))
}

func (store *Store) publish(ctx context.Context, object Object) error {
	for attempt := 0; attempt < 3; attempt++ {
		exists, matches, err := store.inspect(ctx, object)
		if err != nil {
			return errors.New("read existing S3 object")
		}
		if exists {
			if matches {
				return nil
			}
			return ErrConflict
		}
		if _, err := object.Body.Seek(0, io.SeekStart); err != nil {
			return errors.New("rewind object for S3 delivery")
		}
		_, err = store.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(store.bucket),
			Key:           aws.String(object.Key),
			Body:          object.Body,
			ContentLength: aws.Int64(object.Size),
			IfNoneMatch:   aws.String("*"),
		})
		if err == nil {
			exists, matches, readErr := store.inspect(ctx, object)
			if readErr != nil || !exists {
				return errors.New("verify written S3 object")
			}
			if !matches {
				return ErrConflict
			}
			return nil
		}
		status := statusCode(err)
		if status == http.StatusPreconditionFailed || status == http.StatusConflict {
			exists, matches, getErr := store.inspect(ctx, object)
			if getErr == nil && exists && matches {
				return nil
			}
			if getErr == nil && exists && !matches {
				return ErrConflict
			}
			if status == http.StatusConflict {
				continue
			}
			return errors.New("read existing S3 object")
		}
		return errors.New("write S3 object")
	}
	return errors.New("write S3 object after conditional-write conflict")
}

func (store *Store) inspect(ctx context.Context, object Object) (bool, bool, error) {
	result, err := store.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(store.bucket), Key: aws.String(object.Key)})
	if err != nil {
		if statusCode(err) == http.StatusNotFound {
			return false, false, nil
		}
		return false, false, err
	}
	defer result.Body.Close()
	existingHash := sha256.New()
	existingSize, err := io.Copy(existingHash, result.Body)
	if err != nil {
		return true, false, err
	}
	if _, err := object.Body.Seek(0, io.SeekStart); err != nil {
		return true, false, err
	}
	wantedHash := sha256.New()
	wantedSize, err := io.Copy(wantedHash, object.Body)
	if err != nil {
		return true, false, err
	}
	return true, existingSize == object.Size && wantedSize == object.Size && subtle.ConstantTimeCompare(existingHash.Sum(nil), wantedHash.Sum(nil)) == 1, nil
}

func statusCode(err error) int {
	var responseError *smithyhttp.ResponseError
	if errors.As(err, &responseError) {
		return responseError.HTTPStatusCode()
	}
	return 0
}
