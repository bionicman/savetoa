// Package status builds deterministic, read-only backup repository reports.
package status

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bionicman/savetoa/internal/localstore"
	"github.com/bionicman/savetoa/internal/manifest"
)

const SchemaVersion = 1

type Repository interface {
	Name() string
	Driver() string
	ListCompleted(context.Context, string, string) ([]localstore.Set, error)
}

type Backup struct {
	BackupID     string    `json:"backup_id"`
	RelativePath string    `json:"relative_path"`
	StartedAt    time.Time `json:"started_at"`
	CompletedAt  time.Time `json:"completed_at"`
	SizeBytes    int64     `json:"size_bytes"`
	Repositories []string  `json:"repositories"`
}

type RepositoryReport struct {
	Name          string `json:"name"`
	Driver        string `json:"driver"`
	CompletedSets int    `json:"completed_sets"`
	LatestBackup  string `json:"latest_backup_id,omitempty"`
}

type Report struct {
	SchemaVersion  int                `json:"schema_version"`
	GeneratedAt    time.Time          `json:"generated_at"`
	Environment    string             `json:"environment"`
	Target         string             `json:"target"`
	Status         string             `json:"status"`
	LatestBackupID string             `json:"latest_backup_id,omitempty"`
	AgeSeconds     *int64             `json:"age_seconds,omitempty"`
	Repositories   []RepositoryReport `json:"repositories"`
	Backups        []Backup           `json:"backups"`
}

type indexedBackup struct {
	set          localstore.Set
	manifestData string
	repositories map[string]struct{}
}

func Inspect(ctx context.Context, environment, target string, repositories []Repository, now time.Time) (*Report, error) {
	if ctx == nil {
		return nil, errors.New("context is required")
	}
	if environment == "" || target == "" {
		return nil, errors.New("environment and target are required")
	}
	if len(repositories) == 0 {
		return nil, errors.New("at least one status repository is required")
	}
	if now.IsZero() {
		return nil, errors.New("report time is required")
	}

	report := &Report{
		SchemaVersion: SchemaVersion,
		GeneratedAt:   now.UTC(),
		Environment:   environment,
		Target:        target,
		Status:        "empty",
		Repositories:  make([]RepositoryReport, 0, len(repositories)),
		Backups:       []Backup{},
	}
	ordered := append([]Repository(nil), repositories...)
	slices.SortFunc(ordered, func(left, right Repository) int {
		if left == nil || right == nil {
			return 0
		}
		return strings.Compare(left.Name(), right.Name())
	})
	seenRepositories := make(map[string]struct{}, len(ordered))
	indexed := make(map[string]*indexedBackup)
	for _, repository := range ordered {
		if repository == nil || repository.Name() == "" || repository.Driver() == "" {
			return nil, errors.New("status repository name and driver are required")
		}
		if _, duplicate := seenRepositories[repository.Name()]; duplicate {
			return nil, fmt.Errorf("status repository %q is duplicated", repository.Name())
		}
		seenRepositories[repository.Name()] = struct{}{}
		sets, err := repository.ListCompleted(ctx, environment, target)
		if err != nil {
			return nil, fmt.Errorf("scan status repository %q: %w", repository.Name(), err)
		}
		slices.SortFunc(sets, compareSetsNewestFirst)
		repositoryReport := RepositoryReport{Name: repository.Name(), Driver: repository.Driver(), CompletedSets: len(sets)}
		if len(sets) > 0 {
			repositoryReport.LatestBackup = sets[0].Manifest.BackupID
		}
		report.Repositories = append(report.Repositories, repositoryReport)
		seenInRepository := make(map[string]struct{}, len(sets))
		for _, set := range sets {
			backupID := set.Manifest.BackupID
			if _, duplicate := seenInRepository[backupID]; duplicate {
				return nil, fmt.Errorf("status repository %q contains duplicate backup ID %q", repository.Name(), backupID)
			}
			seenInRepository[backupID] = struct{}{}
			data, err := manifest.Marshal(set.Manifest)
			if err != nil {
				return nil, fmt.Errorf("encode status manifest %q: %w", backupID, err)
			}
			current, exists := indexed[backupID]
			if !exists {
				indexed[backupID] = &indexedBackup{set: set, manifestData: string(data), repositories: map[string]struct{}{repository.Name(): {}}}
				continue
			}
			if current.set.RelativePath != set.RelativePath || current.manifestData != string(data) {
				return nil, fmt.Errorf("backup ID %q has conflicting metadata across repositories", backupID)
			}
			current.repositories[repository.Name()] = struct{}{}
		}
	}

	for _, indexedSet := range indexed {
		repositoryNames := make([]string, 0, len(indexedSet.repositories))
		for name := range indexedSet.repositories {
			repositoryNames = append(repositoryNames, name)
		}
		slices.Sort(repositoryNames)
		report.Backups = append(report.Backups, Backup{
			BackupID: indexedSet.set.Manifest.BackupID, RelativePath: indexedSet.set.RelativePath,
			StartedAt: indexedSet.set.Manifest.StartedAt, CompletedAt: indexedSet.set.Manifest.CompletedAt,
			SizeBytes: indexedSet.set.Manifest.Artifact.SizeBytes, Repositories: repositoryNames,
		})
	}
	slices.SortFunc(report.Backups, func(left, right Backup) int {
		if order := right.CompletedAt.Compare(left.CompletedAt); order != 0 {
			return order
		}
		return strings.Compare(right.BackupID, left.BackupID)
	})
	if len(report.Backups) == 0 {
		return report, nil
	}
	latest := report.Backups[0]
	report.LatestBackupID = latest.BackupID
	age := int64(now.UTC().Sub(latest.CompletedAt.UTC()) / time.Second)
	report.AgeSeconds = &age
	if len(latest.Repositories) == len(report.Repositories) {
		report.Status = "complete"
	} else {
		report.Status = "degraded"
	}
	return report, nil
}

func compareSetsNewestFirst(left, right localstore.Set) int {
	if order := right.Manifest.CompletedAt.Compare(left.Manifest.CompletedAt); order != 0 {
		return order
	}
	return strings.Compare(right.Manifest.BackupID, left.Manifest.BackupID)
}
