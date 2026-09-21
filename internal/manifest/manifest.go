// Package manifest defines the untrusted, versioned backup-set manifest.
package manifest

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	FormatVersion = 1
	MaxFileSize   = 1 << 20
)

var (
	identifierPattern           = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
	targetPattern               = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	artifactFilenamePattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,254}$`)
	metadataKeyPattern          = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
	recipientFingerprintPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

type Manifest struct {
	FormatVersion   int              `json:"format_version"`
	BackupID        string           `json:"backup_id"`
	Target          string           `json:"target"`
	CaptureDriver   string           `json:"capture_driver"`
	StartedAt       time.Time        `json:"started_at"`
	CompletedAt     time.Time        `json:"completed_at"`
	Tool            Tool             `json:"tool"`
	Source          Source           `json:"source"`
	Artifact        Artifact         `json:"artifact"`
	Transformations []Transformation `json:"transformations"`
}

type Tool struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type Source struct {
	ServerVersion string            `json:"server_version"`
	Replication   map[string]string `json:"replication"`
}

type Artifact struct {
	Filename  string   `json:"filename"`
	SizeBytes int64    `json:"size_bytes"`
	Checksum  Checksum `json:"checksum"`
}

type Checksum struct {
	Algorithm string `json:"algorithm"`
	Value     string `json:"value"`
}

type Transformation struct {
	Driver                string   `json:"driver"`
	Level                 *int     `json:"level,omitempty"`
	RecipientFingerprints []string `json:"recipient_fingerprints,omitempty"`
}

func Read(reader io.Reader) (*Manifest, error) {
	data, err := io.ReadAll(io.LimitReader(reader, MaxFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	if len(data) > MaxFileSize {
		return nil, fmt.Errorf("manifest exceeds %d bytes", MaxFileSize)
	}
	return Parse(data)
}

func Parse(data []byte) (*Manifest, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errors.New("manifest is empty")
	}
	if err := rejectDuplicateKeys(data); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	var result Manifest
	if err := decoder.Decode(&result); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("manifest must contain exactly one JSON value")
		}
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if err := result.Validate(); err != nil {
		return nil, err
	}
	return &result, nil
}

func Marshal(value Manifest) ([]byte, error) {
	if err := value.Validate(); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode manifest: %w", err)
	}
	return append(data, '\n'), nil
}

func (manifest Manifest) Validate() error {
	if manifest.FormatVersion != FormatVersion {
		return fmt.Errorf("format_version must be %d", FormatVersion)
	}
	if !identifierPattern.MatchString(manifest.BackupID) {
		return fmt.Errorf("backup_id must match %s", identifierPattern.String())
	}
	if !targetPattern.MatchString(manifest.Target) {
		return fmt.Errorf("target must match %s", targetPattern.String())
	}
	switch manifest.CaptureDriver {
	case "mariadb", "mongodb", "redis", "sqlite3", "tar", "garage":
	default:
		return fmt.Errorf("unsupported capture_driver %q", manifest.CaptureDriver)
	}
	if manifest.StartedAt.IsZero() || manifest.CompletedAt.IsZero() {
		return errors.New("started_at and completed_at are required")
	}
	if manifest.CompletedAt.Before(manifest.StartedAt) {
		return errors.New("completed_at must not precede started_at")
	}
	if manifest.Tool.Name == "" || manifest.Tool.Version == "" {
		return errors.New("tool.name and tool.version are required")
	}
	if manifest.CaptureDriver != "tar" && manifest.Source.ServerVersion == "" {
		return errors.New("source.server_version is required")
	}
	if manifest.Source.Replication == nil {
		return errors.New("source.replication must be an object")
	}
	if manifest.CaptureDriver == "mariadb" || manifest.CaptureDriver == "mongodb" || manifest.CaptureDriver == "redis" {
		if len(manifest.Source.Replication) == 0 {
			return errors.New("source.replication is required for database captures")
		}
	}
	for key, value := range manifest.Source.Replication {
		if !metadataKeyPattern.MatchString(key) {
			return fmt.Errorf("source.replication key %q must match %s", key, metadataKeyPattern.String())
		}
		if isSensitiveMetadataKey(key) {
			return fmt.Errorf("source.replication key %q is reserved for secret-bearing data", key)
		}
		if value == "" || len(value) > 1024 {
			return fmt.Errorf("source.replication value for %q must contain 1 to 1024 bytes", key)
		}
	}
	if err := manifest.Artifact.validate(); err != nil {
		return err
	}
	if manifest.Transformations == nil {
		return errors.New("transformations must be an array")
	}
	if err := validateTransformations(manifest.Transformations); err != nil {
		return err
	}
	return nil
}

func isSensitiveMetadataKey(key string) bool {
	if key == "access_key" || key == "private_key" || key == "auth" {
		return true
	}
	for _, fragment := range []string{"password", "passwd", "secret", "token", "credential"} {
		if strings.Contains(key, fragment) {
			return true
		}
	}
	return false
}

func (artifact Artifact) validate() error {
	if !artifactFilenamePattern.MatchString(artifact.Filename) || artifact.Filename == "." || artifact.Filename == ".." ||
		filepath.Base(artifact.Filename) != artifact.Filename ||
		strings.ContainsAny(artifact.Filename, `/\\`) || artifact.Filename == "manifest.json" || artifact.Filename == "complete" {
		return errors.New("artifact.filename must be a single safe path component")
	}
	if artifact.SizeBytes <= 0 {
		return errors.New("artifact.size_bytes must be positive")
	}
	if artifact.Checksum.Algorithm != "sha256" {
		return errors.New("artifact.checksum.algorithm must be sha256")
	}
	decoded, err := hex.DecodeString(artifact.Checksum.Value)
	if err != nil || len(decoded) != 32 || strings.ToLower(artifact.Checksum.Value) != artifact.Checksum.Value {
		return errors.New("artifact.checksum.value must be a lowercase SHA-256 digest")
	}
	return nil
}

func validateTransformations(transformations []Transformation) error {
	seen := make(map[string]struct{}, len(transformations))
	ageSeen := false
	for index, transformation := range transformations {
		if _, ok := seen[transformation.Driver]; ok {
			return fmt.Errorf("transformations[%d]: duplicate driver %q", index, transformation.Driver)
		}
		seen[transformation.Driver] = struct{}{}

		switch transformation.Driver {
		case "zstd":
			if ageSeen {
				return fmt.Errorf("transformations[%d]: compression must precede encryption", index)
			}
			if transformation.Level == nil || *transformation.Level < -5 || *transformation.Level > 22 {
				return fmt.Errorf("transformations[%d]: zstd level must be between -5 and 22", index)
			}
			if len(transformation.RecipientFingerprints) != 0 {
				return fmt.Errorf("transformations[%d]: zstd contains age-only metadata", index)
			}
		case "age":
			ageSeen = true
			if transformation.Level != nil {
				return fmt.Errorf("transformations[%d]: age contains zstd-only metadata", index)
			}
			if len(transformation.RecipientFingerprints) == 0 {
				return fmt.Errorf("transformations[%d]: age recipient fingerprints are required", index)
			}
			for _, fingerprint := range transformation.RecipientFingerprints {
				if !recipientFingerprintPattern.MatchString(fingerprint) {
					return fmt.Errorf("transformations[%d]: invalid age recipient fingerprint", index)
				}
			}
		default:
			return fmt.Errorf("transformations[%d]: unsupported driver %q", index, transformation.Driver)
		}
	}
	return nil
}

func rejectDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := scanJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("manifest must contain exactly one JSON value")
		}
		return err
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}

	switch delimiter {
	case '{':
		keys := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, exists := keys[key]; exists {
				return fmt.Errorf("duplicate object key %q", key)
			}
			keys[key] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
}
