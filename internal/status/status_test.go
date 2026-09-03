package status

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bionicman/savetoa/internal/localstore"
	"github.com/bionicman/savetoa/internal/manifest"
)

type fakeRepository struct {
	name   string
	driver string
	sets   []localstore.Set
	err    error
}

func (repository *fakeRepository) Name() string   { return repository.name }
func (repository *fakeRepository) Driver() string { return repository.driver }
func (repository *fakeRepository) ListCompleted(context.Context, string, string) ([]localstore.Set, error) {
	return append([]localstore.Set(nil), repository.sets...), repository.err
}

func TestInspectReportsLatestCompleteBackup(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	old := statusSet("old", now.Add(-25*time.Hour))
	latest := statusSet("latest", now.Add(-time.Hour))
	report, err := Inspect(context.Background(), "production", "database", []Repository{
		&fakeRepository{name: "spool", driver: "local", sets: []localstore.Set{old, latest}},
		&fakeRepository{name: "offsite", driver: "s3", sets: []localstore.Set{latest}},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "complete" || report.LatestBackupID != "latest" || report.AgeSeconds == nil || *report.AgeSeconds != 3600 {
		t.Fatalf("report = %#v", report)
	}
	if len(report.Backups) != 2 || report.Backups[0].BackupID != "latest" || len(report.Backups[0].Repositories) != 2 {
		t.Fatalf("backups = %#v", report.Backups)
	}
	if report.Repositories[0].Name != "offsite" || report.Repositories[1].Name != "spool" {
		t.Fatalf("repositories are not deterministic: %#v", report.Repositories)
	}
}

func TestInspectReportsEmptyAndDegradedRepositories(t *testing.T) {
	now := time.Now().UTC()
	tests := map[string]struct {
		repositories []Repository
		want         string
	}{
		"empty": {
			repositories: []Repository{&fakeRepository{name: "spool", driver: "local"}},
			want:         "empty",
		},
		"degraded": {
			repositories: []Repository{
				&fakeRepository{name: "spool", driver: "local", sets: []localstore.Set{statusSet("latest", now.Add(-time.Hour))}},
				&fakeRepository{name: "offsite", driver: "s3"},
			},
			want: "degraded",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			report, err := Inspect(context.Background(), "production", "database", test.repositories, now)
			if err != nil {
				t.Fatal(err)
			}
			if report.Status != test.want {
				t.Fatalf("status = %q, want %q", report.Status, test.want)
			}
		})
	}
}

func TestInspectFailsClosed(t *testing.T) {
	now := time.Now().UTC()
	value := statusSet("same", now.Add(-time.Hour))
	conflict := value
	conflict.Manifest.Artifact.SizeBytes++
	tests := map[string][]Repository{
		"scan failure": {
			&fakeRepository{name: "spool", driver: "local", err: errors.New("unavailable")},
		},
		"duplicate repository": {
			&fakeRepository{name: "spool", driver: "local"},
			&fakeRepository{name: "spool", driver: "local"},
		},
		"conflicting metadata": {
			&fakeRepository{name: "spool", driver: "local", sets: []localstore.Set{value}},
			&fakeRepository{name: "offsite", driver: "s3", sets: []localstore.Set{conflict}},
		},
	}
	for name, repositories := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Inspect(context.Background(), "production", "database", repositories, now); err == nil {
				t.Fatal("Inspect() succeeded")
			}
		})
	}
}

func statusSet(backupID string, completed time.Time) localstore.Set {
	return localstore.Set{
		Environment: "production", RelativePath: "production/database/2026/09/03/" + backupID,
		Manifest: manifest.Manifest{
			FormatVersion: manifest.FormatVersion, BackupID: backupID, Target: "database", CaptureDriver: "mariadb",
			StartedAt: completed.Add(-time.Minute), CompletedAt: completed,
			Tool:   manifest.Tool{Name: "mariadb-backup", Version: "12.3.3"},
			Source: manifest.Source{ServerVersion: "12.3.3", Replication: map[string]string{"gtid": "0-1-2"}},
			Artifact: manifest.Artifact{Filename: "payload.tar", SizeBytes: 7,
				Checksum: manifest.Checksum{Algorithm: "sha256", Value: "239f59ed55e737c77147cf55ad0c1b030b6d7ee748a7426952f9b852d5a935e5"}},
			Transformations: []manifest.Transformation{},
		},
	}
}
