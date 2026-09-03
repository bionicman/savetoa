package retention

import (
	"slices"
	"testing"
	"time"

	"github.com/bionicman/savetoa/internal/config"
)

func TestBuildKeepsUnionOfNewestGFSBuckets(t *testing.T) {
	candidates := []Candidate{
		candidate("newest", "2026-03-02T12:00:00Z"),
		candidate("same-day-older", "2026-03-02T08:00:00Z"),
		candidate("previous-day", "2026-03-01T12:00:00Z"),
		candidate("previous-week", "2026-02-22T12:00:00Z"),
		candidate("previous-month", "2026-01-31T12:00:00Z"),
	}
	plan, err := Build(candidates, config.Retention{KeepDaily: 2, KeepWeekly: 2, KeepMonthly: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ids(plan.Keep), []string{"newest", "previous-day", "previous-week"}; !slices.Equal(got, want) {
		t.Fatalf("kept IDs = %#v, want %#v", got, want)
	}
	if got, want := ids(plan.Prune), []string{"previous-month", "same-day-older"}; !slices.Equal(got, want) {
		t.Fatalf("pruned IDs = %#v, want %#v", got, want)
	}
}

func TestBuildUsesUTCAndISOWeeks(t *testing.T) {
	newest := candidate("newest", "2027-01-01T00:05:00Z")
	olderSameISOWeek := candidate("same-week", "2026-12-31T23:55:00Z")
	olderSameISOWeek.StartedAt = olderSameISOWeek.StartedAt.In(time.FixedZone("west", -2*60*60))
	plan, err := Build([]Candidate{olderSameISOWeek, newest}, config.Retention{KeepWeekly: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ids(plan.Keep), []string{"newest"}; !slices.Equal(got, want) {
		t.Fatalf("kept IDs = %#v, want %#v", got, want)
	}
}

func TestBuildRejectsUnsafeInput(t *testing.T) {
	value := candidate("duplicate", "2026-03-02T12:00:00Z")
	if _, err := Build([]Candidate{value}, config.Retention{}); err == nil {
		t.Fatal("empty retention policy was accepted")
	}
	if _, err := Build([]Candidate{value, value}, config.Retention{KeepDaily: 1}); err == nil {
		t.Fatal("duplicate candidate was accepted")
	}
}

func candidate(id, timestamp string) Candidate {
	started, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		panic(err)
	}
	return Candidate{BackupID: id, RelativePath: "test/example/2026/01/01/" + id, StartedAt: started}
}

func ids(candidates []Candidate) []string {
	result := make([]string, len(candidates))
	for index, candidate := range candidates {
		result[index] = candidate.BackupID
	}
	return result
}
