package prune

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bionicman/savetoa/internal/config"
	"github.com/bionicman/savetoa/internal/localstore"
	"github.com/bionicman/savetoa/internal/manifest"
)

type fakeRepository struct {
	name    string
	sets    []localstore.Set
	listErr error
	deleted []string
}

func (repository *fakeRepository) Name() string { return repository.name }

func (repository *fakeRepository) ListCompleted(context.Context, string, string) ([]localstore.Set, error) {
	if repository.listErr != nil {
		return nil, repository.listErr
	}
	return append([]localstore.Set(nil), repository.sets...), nil
}

func (repository *fakeRepository) DeleteCompleted(_ context.Context, selected localstore.Set) error {
	repository.deleted = append(repository.deleted, selected.Manifest.BackupID)
	for index, set := range repository.sets {
		if set.RelativePath == selected.RelativePath {
			repository.sets = append(repository.sets[:index], repository.sets[index+1:]...)
			return nil
		}
	}
	return errors.New("set not found")
}

func TestExecutePlansEveryRepositoryBeforeDeleting(t *testing.T) {
	valid := &fakeRepository{name: "local", sets: []localstore.Set{set("new", 2), set("old", 1)}}
	invalid := &fakeRepository{name: "offsite", listErr: errors.New("invalid completion marker")}
	if _, err := Execute(context.Background(), "test", "database", config.Retention{KeepDaily: 1}, []Repository{valid, invalid}); err == nil {
		t.Fatal("Execute(invalid repository) succeeded")
	}
	if len(valid.deleted) != 0 {
		t.Fatalf("sets were deleted before all plans succeeded: %#v", valid.deleted)
	}
}

func TestExecuteAppliesPlansIndependently(t *testing.T) {
	local := &fakeRepository{name: "local", sets: []localstore.Set{set("new", 2), set("old", 1)}}
	offsite := &fakeRepository{name: "offsite", sets: []localstore.Set{set("new", 2)}}
	results, err := Execute(context.Background(), "test", "database", config.Retention{KeepDaily: 1}, []Repository{local, offsite})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Pruned != 1 || results[1].Pruned != 0 || len(local.deleted) != 1 || local.deleted[0] != "old" {
		t.Fatalf("results=%#v deleted=%#v", results, local.deleted)
	}
}

func set(id string, day int) localstore.Set {
	started := time.Date(2026, 9, day, 12, 0, 0, 0, time.UTC)
	return localstore.Set{
		Environment: "test", RelativePath: "test/database/2026/09/0" + string(rune('0'+day)) + "/" + id,
		Manifest: manifest.Manifest{BackupID: id, Target: "database", StartedAt: started},
	}
}
