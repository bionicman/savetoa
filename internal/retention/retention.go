// Package retention selects completed backup sets to keep and prune.
package retention

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/bionicman/savetoa/internal/config"
)

type Candidate struct {
	BackupID     string
	RelativePath string
	StartedAt    time.Time
}

type Plan struct {
	Keep  []Candidate
	Prune []Candidate
}

// Build applies daily, ISO-weekly, and monthly GFS buckets. The newest set in
// each retained UTC bucket is kept, and the union of all retained buckets wins.
func Build(candidates []Candidate, policy config.Retention) (Plan, error) {
	if policy.KeepDaily < 0 || policy.KeepWeekly < 0 || policy.KeepMonthly < 0 ||
		(policy.KeepDaily == 0 && policy.KeepWeekly == 0 && policy.KeepMonthly == 0) {
		return Plan{}, errors.New("retention policy must keep at least one completed backup")
	}
	ordered := append([]Candidate(nil), candidates...)
	seenIDs := make(map[string]struct{}, len(ordered))
	seenPaths := make(map[string]struct{}, len(ordered))
	for _, candidate := range ordered {
		if candidate.BackupID == "" || candidate.RelativePath == "" || candidate.StartedAt.IsZero() {
			return Plan{}, errors.New("retention candidate is incomplete")
		}
		if _, exists := seenIDs[candidate.BackupID]; exists {
			return Plan{}, fmt.Errorf("retention candidate backup ID %q is duplicated", candidate.BackupID)
		}
		if _, exists := seenPaths[candidate.RelativePath]; exists {
			return Plan{}, fmt.Errorf("retention candidate path %q is duplicated", candidate.RelativePath)
		}
		seenIDs[candidate.BackupID] = struct{}{}
		seenPaths[candidate.RelativePath] = struct{}{}
	}
	sort.Slice(ordered, func(left, right int) bool {
		if !ordered[left].StartedAt.Equal(ordered[right].StartedAt) {
			return ordered[left].StartedAt.After(ordered[right].StartedAt)
		}
		return ordered[left].BackupID > ordered[right].BackupID
	})

	keep := make(map[string]struct{}, len(ordered))
	selectBuckets(ordered, policy.KeepDaily, func(value time.Time) string {
		return value.UTC().Format("2006-01-02")
	}, keep)
	selectBuckets(ordered, policy.KeepWeekly, func(value time.Time) string {
		year, week := value.UTC().ISOWeek()
		return fmt.Sprintf("%04d-W%02d", year, week)
	}, keep)
	selectBuckets(ordered, policy.KeepMonthly, func(value time.Time) string {
		return value.UTC().Format("2006-01")
	}, keep)

	plan := Plan{}
	for _, candidate := range ordered {
		if _, ok := keep[candidate.RelativePath]; ok {
			plan.Keep = append(plan.Keep, candidate)
		} else {
			plan.Prune = append(plan.Prune, candidate)
		}
	}
	// Delete oldest sets first while keeping output deterministic.
	for left, right := 0, len(plan.Prune)-1; left < right; left, right = left+1, right-1 {
		plan.Prune[left], plan.Prune[right] = plan.Prune[right], plan.Prune[left]
	}
	return plan, nil
}

func selectBuckets(candidates []Candidate, limit int, bucket func(time.Time) string, keep map[string]struct{}) {
	if limit == 0 {
		return
	}
	selected := make(map[string]struct{}, limit)
	for _, candidate := range candidates {
		key := bucket(candidate.StartedAt)
		if _, exists := selected[key]; exists {
			continue
		}
		if len(selected) == limit {
			return
		}
		selected[key] = struct{}{}
		keep[candidate.RelativePath] = struct{}{}
	}
}
