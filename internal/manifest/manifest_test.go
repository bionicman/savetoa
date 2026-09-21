package manifest

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMarshalParseRoundTrip(t *testing.T) {
	want := validManifest()
	data, err := Marshal(want)
	if err != nil {
		t.Fatalf("Marshal(valid) error = %v", err)
	}
	got, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse(Marshal(valid)) error = %v", err)
	}
	if got.BackupID != want.BackupID || got.Artifact.Checksum != want.Artifact.Checksum {
		t.Fatalf("round trip mismatch: got %#v, want %#v", got, want)
	}
}

func TestDocumentedExampleMatchesSchema(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "manifest-v1.example.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	if _, err := Parse(data); err != nil {
		t.Fatalf("Parse(documented example) error = %v", err)
	}
}

func TestParseRejectsUnknownAndDuplicateFields(t *testing.T) {
	data, err := Marshal(validManifest())
	if err != nil {
		t.Fatal(err)
	}

	unknown := bytes.Replace(data, []byte(`"format_version": 1,`), []byte(`"format_version": 1, "secret": "forbidden",`), 1)
	if _, err := Parse(unknown); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("Parse(unknown field) error = %v", err)
	}

	duplicate := bytes.Replace(data, []byte(`"backup_id": "20260902-example",`), []byte(`"backup_id": "first", "backup_id": "20260902-example",`), 1)
	if _, err := Parse(duplicate); err == nil || !strings.Contains(err.Error(), "duplicate object key") {
		t.Fatalf("Parse(duplicate field) error = %v", err)
	}
}

func TestParseFailsClosed(t *testing.T) {
	tests := map[string]struct {
		mutate func(*Manifest)
		want   string
	}{
		"path traversal": {
			mutate: func(value *Manifest) { value.Artifact.Filename = "../payload.tar.zst" },
			want:   "safe path component",
		},
		"reserved completion filename": {
			mutate: func(value *Manifest) { value.Artifact.Filename = "complete" },
			want:   "safe path component",
		},
		"uppercase checksum": {
			mutate: func(value *Manifest) { value.Artifact.Checksum.Value = strings.Repeat("A", 64) },
			want:   "lowercase SHA-256",
		},
		"completion before start": {
			mutate: func(value *Manifest) { value.CompletedAt = value.StartedAt.Add(-time.Second) },
			want:   "must not precede",
		},
		"unknown capture driver": {
			mutate: func(value *Manifest) { value.CaptureDriver = "command" },
			want:   "unsupported capture_driver",
		},
		"missing replication metadata": {
			mutate: func(value *Manifest) { value.Source.Replication = map[string]string{} },
			want:   "required for database captures",
		},
		"secret replication metadata": {
			mutate: func(value *Manifest) { value.Source.Replication["access_token"] = "forbidden" },
			want:   "reserved for secret-bearing data",
		},
		"nil transformations": {
			mutate: func(value *Manifest) { value.Transformations = nil },
			want:   "must be an array",
		},
		"encryption before compression": {
			mutate: func(value *Manifest) {
				value.Transformations[0], value.Transformations[1] = value.Transformations[1], value.Transformations[0]
			},
			want: "compression must precede encryption",
		},
		"invalid recipient fingerprint": {
			mutate: func(value *Manifest) { value.Transformations[1].RecipientFingerprints[0] = "age1-public-key" },
			want:   "invalid age recipient fingerprint",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			value := validManifest()
			test.mutate(&value)
			data, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			_, err = Parse(data)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Parse(invalid) error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestParseRejectsMultipleJSONValues(t *testing.T) {
	data, err := Marshal(validManifest())
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, data...)
	if _, err := Parse(data); err == nil || !strings.Contains(err.Error(), "exactly one JSON value") {
		t.Fatalf("Parse(multiple values) error = %v", err)
	}
}

func TestTarManifestAllowsNoServerOrReplicationMetadata(t *testing.T) {
	value := validManifest()
	value.Target = "example-files"
	value.CaptureDriver = "tar"
	value.Tool = Tool{Name: "tar", Version: "1.35"}
	value.Source = Source{Replication: map[string]string{}}
	data, err := Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(data); err != nil {
		t.Fatalf("Parse(tar manifest) error = %v", err)
	}
}

func TestSQLite3ManifestRequiresVersionButNotReplicationMetadata(t *testing.T) {
	value := validManifest()
	value.Target = "example-sqlite"
	value.CaptureDriver = "sqlite3"
	value.Tool = Tool{Name: "sqlite3", Version: "3.46.1"}
	value.Source = Source{ServerVersion: "3.46.1", Replication: map[string]string{}}
	if _, err := Marshal(value); err != nil {
		t.Fatalf("Marshal(SQLite3 manifest) error = %v", err)
	}
	value.Source.ServerVersion = ""
	if _, err := Marshal(value); err == nil || !strings.Contains(err.Error(), "server_version") {
		t.Fatalf("Marshal(SQLite3 manifest without version) error = %v", err)
	}
}

func TestReadRejectsOversizedManifest(t *testing.T) {
	data := strings.Repeat("x", MaxFileSize+1)
	_, err := Read(strings.NewReader(data))
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("Read(oversized) error = %v, want size error", err)
	}
}

func validManifest() Manifest {
	level := 3
	return Manifest{
		FormatVersion: FormatVersion,
		BackupID:      "20260902-example",
		Target:        "example-mariadb",
		CaptureDriver: "mariadb",
		StartedAt:     time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC),
		CompletedAt:   time.Date(2026, 9, 2, 10, 5, 0, 0, time.UTC),
		Tool: Tool{
			Name:    "mariadb-backup",
			Version: "12.3.0",
		},
		Source: Source{
			ServerVersion: "12.3.0",
			Replication: map[string]string{
				"gtid": "0-1-42",
			},
		},
		Artifact: Artifact{
			Filename:  "payload.tar.zst.age",
			SizeBytes: 1024,
			Checksum: Checksum{
				Algorithm: "sha256",
				Value:     strings.Repeat("a", 64),
			},
		},
		Transformations: []Transformation{
			{Driver: "zstd", Level: &level},
			{Driver: "age", RecipientFingerprints: []string{"sha256:" + strings.Repeat("b", 64)}},
		},
	}
}
