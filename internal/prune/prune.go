// Package prune plans and applies retention independently to backup repositories.
package prune

import (
	"context"
	"errors"
	"fmt"

	"github.com/bionicman/savetoa/internal/config"
	"github.com/bionicman/savetoa/internal/localstore"
	"github.com/bionicman/savetoa/internal/retention"
	"github.com/bionicman/savetoa/internal/s3store"
)

type Repository interface {
	Name() string
	ListCompleted(context.Context, string, string) ([]localstore.Set, error)
	DeleteCompleted(context.Context, localstore.Set) error
}

type Result struct {
	Repository string
	Kept       int
	Pruned     int
}

type repositoryPlan struct {
	repository Repository
	plan       retention.Plan
	sets       map[string]localstore.Set
}

// Execute validates every repository and builds every plan before the first
// destructive operation. Repositories are then pruned independently in the
// supplied deterministic order.
func Execute(ctx context.Context, environment, target string, policy config.Retention, repositories []Repository) ([]Result, error) {
	if ctx == nil {
		return nil, errors.New("context is required")
	}
	if len(repositories) == 0 {
		return nil, errors.New("at least one retention repository is required")
	}
	plans := make([]repositoryPlan, 0, len(repositories))
	seen := make(map[string]struct{}, len(repositories))
	for _, repository := range repositories {
		if repository == nil || repository.Name() == "" {
			return nil, errors.New("retention repository and name are required")
		}
		if _, exists := seen[repository.Name()]; exists {
			return nil, fmt.Errorf("retention repository %q is duplicated", repository.Name())
		}
		seen[repository.Name()] = struct{}{}
		sets, err := repository.ListCompleted(ctx, environment, target)
		if err != nil {
			return nil, fmt.Errorf("scan retention repository %q: %w", repository.Name(), err)
		}
		candidates := make([]retention.Candidate, len(sets))
		setsByPath := make(map[string]localstore.Set, len(sets))
		for index, set := range sets {
			candidates[index] = retention.Candidate{
				BackupID: set.Manifest.BackupID, RelativePath: set.RelativePath, StartedAt: set.Manifest.StartedAt,
			}
			setsByPath[set.RelativePath] = set
		}
		plan, err := retention.Build(candidates, policy)
		if err != nil {
			return nil, fmt.Errorf("plan retention repository %q: %w", repository.Name(), err)
		}
		plans = append(plans, repositoryPlan{repository: repository, plan: plan, sets: setsByPath})
	}

	results := make([]Result, 0, len(plans))
	for _, planned := range plans {
		for _, candidate := range planned.plan.Prune {
			set, found := planned.sets[candidate.RelativePath]
			if !found {
				return nil, errors.New("retention plan lost a selected set")
			}
			if err := planned.repository.DeleteCompleted(ctx, set); err != nil {
				return nil, fmt.Errorf("prune repository %q set %q: %w", planned.repository.Name(), candidate.BackupID, err)
			}
		}
		results = append(results, Result{Repository: planned.repository.Name(), Kept: len(planned.plan.Keep), Pruned: len(planned.plan.Prune)})
	}
	return results, nil
}

type LocalRepository struct {
	RepositoryName string
	Store          *localstore.Store
}

func (repository *LocalRepository) Name() string { return repository.RepositoryName }

func (repository *LocalRepository) ListCompleted(_ context.Context, environment, target string) ([]localstore.Set, error) {
	if repository == nil || repository.Store == nil {
		return nil, errors.New("local retention store is required")
	}
	return repository.Store.ListCompleted(environment, target)
}

func (repository *LocalRepository) DeleteCompleted(ctx context.Context, set localstore.Set) error {
	return repository.Store.DeleteCompleted(ctx, set)
}

type S3Repository struct {
	RepositoryName string
	Store          *s3store.Store
}

func (repository *S3Repository) Name() string { return repository.RepositoryName }

func (repository *S3Repository) ListCompleted(ctx context.Context, environment, target string) ([]localstore.Set, error) {
	if repository == nil || repository.Store == nil {
		return nil, errors.New("S3 retention store is required")
	}
	return repository.Store.ListCompleted(ctx, environment, target)
}

func (repository *S3Repository) DeleteCompleted(ctx context.Context, set localstore.Set) error {
	return repository.Store.DeleteCompleted(ctx, set)
}
