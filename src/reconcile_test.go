package main

import (
	"slices"
	"strings"
	"testing"
)

func rec(id, name, dest string) Record {
	yes := true
	return Record{Id: RecordID(id), Name: name, Destination: dest, Modify: &yes}
}

func lockedRec(id, name, dest string) Record {
	no := false
	return Record{Id: RecordID(id), Name: name, Destination: dest, Modify: &no}
}

const cycleIP = "85.253.172.182"

// plan() is the whole policy, so every branch of it gets a row. The dangerous rows are the
// ones where the right answer is to do nothing: those are what protect four unmanaged
// A-records from being duplicated because a listing was missing, empty, or a lie.
func TestPlan(t *testing.T) {
	tests := []struct {
		name          string
		desired       []string
		listings      map[string][]Record
		opts          PlanOptions
		wantActions   []Action
		wantUnchanged int
		wantWarnings  []string // substrings, each expected somewhere in Plan.Warnings
		wantUnmanaged []string
	}{
		{
			name:    "everything already points at this address",
			desired: []string{"stackoverlord.dev", "saturn.stackoverlord.dev"},
			listings: map[string][]Record{"stackoverlord.dev": {
				rec("845134", "stackoverlord.dev", cycleIP),
				rec("856494", "saturn.stackoverlord.dev", cycleIP),
			}},
			wantUnchanged: 2,
		},
		{
			name:    "a stale record is updated with the id from the listing",
			desired: []string{"stackoverlord.dev"},
			listings: map[string][]Record{"stackoverlord.dev": {
				rec("845134", "stackoverlord.dev", "203.0.113.1"),
			}},
			wantActions: []Action{{
				Kind: actionUpdate, Domain: "stackoverlord.dev", Zone: "stackoverlord.dev",
				RecordID: "845134", Current: "203.0.113.1",
			}},
		},
		{
			name:    "a name with no record is created",
			desired: []string{"new.example.org"},
			listings: map[string][]Record{"example.org": {
				rec("1", "other.example.org", "203.0.113.9"),
			}},
			wantActions:   []Action{{Kind: actionCreate, Domain: "new.example.org", Zone: "example.org"}},
			wantUnmanaged: []string{"other.example.org"},
		},
		{
			// The row that matters most. "Could not read it" must never collapse into
			// "there is nothing there", or a transient failure creates duplicates.
			name:     "a zone whose listing failed is left alone",
			desired:  []string{"saturn.stackoverlord.dev"},
			listings: map[string][]Record{
				// deliberately no entry for stackoverlord.dev: that listing failed
			},
			wantWarnings: []string{"no listing available", "nothing was changed"},
		},
		{
			name:         "an empty listing is not trusted",
			desired:      []string{"first.example.org"},
			listings:     map[string][]Record{"example.org": {}},
			wantWarnings: []string{"lists no A-records", "-allow-empty-zone"},
		},
		{
			name:        "an empty listing is used once the operator vouches for the zone",
			desired:     []string{"first.example.org"},
			listings:    map[string][]Record{"example.org": {}},
			opts:        PlanOptions{AllowEmptyZone: true},
			wantActions: []Action{{Kind: actionCreate, Domain: "first.example.org", Zone: "example.org"}},
		},
		{
			name:    "a name with two A-records is not managed",
			desired: []string{"round.example.org"},
			listings: map[string][]Record{"example.org": {
				rec("22", "round.example.org", "203.0.113.1"),
				rec("11", "round.example.org", "203.0.113.2"),
			}},
			wantWarnings: []string{"2 A-records", "11, 22", "keep just the one"},
		},
		{
			name:    "a record Zone will not modify is skipped",
			desired: []string{"locked.example.org"},
			listings: map[string][]Record{"example.org": {
				lockedRec("7", "locked.example.org", "203.0.113.1"),
			}},
			wantWarnings: []string{"not modifiable"},
		},
		{
			// Real shape of one of this project's zones: one managed name among four
			// belonging to other machines.
			name:    "unmanaged records are reported and untouched",
			desired: []string{"stackoverlord.dev"},
			listings: map[string][]Record{"stackoverlord.dev": {
				rec("845134", "stackoverlord.dev", cycleIP),
				rec("845135", "bomberman.stackoverlord.dev", "145.14.30.19"),
				rec("845136", "sql.stackoverlord.dev", "145.14.30.19"),
				rec("845175", "test.stackoverlord.dev", "94.246.238.120"),
			}},
			wantUnchanged: 1,
			wantUnmanaged: []string{"bomberman.stackoverlord.dev", "sql.stackoverlord.dev", "test.stackoverlord.dev"},
		},
		{
			name:    "names match regardless of case",
			desired: []string{"Saturn.example.org"},
			listings: map[string][]Record{"example.org": {
				rec("1", "saturn.example.org", cycleIP),
			}},
			wantUnchanged: 1,
		},
		{
			name:    "several zones produce one action per difference",
			desired: []string{"a.example.org", "b.example.org", "c.other.org"},
			listings: map[string][]Record{
				"example.org": {
					rec("1", "a.example.org", "203.0.113.1"),
					rec("2", "b.example.org", cycleIP),
				},
				"other.org": {},
			},
			opts: PlanOptions{AllowEmptyZone: true},
			wantActions: []Action{
				{Kind: actionUpdate, Domain: "a.example.org", Zone: "example.org", RecordID: "1", Current: "203.0.113.1"},
				{Kind: actionCreate, Domain: "c.other.org", Zone: "other.org"},
			},
			wantUnchanged: 1,
		},
		{
			name:         "a name with no zone is reported, not acted on",
			desired:      []string{"localhost"},
			listings:     map[string][]Record{},
			wantWarnings: []string{"localhost", "at least one dot"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := plan(tc.desired, tc.listings, cycleIP, tc.opts)

			if len(got.Actions) != len(tc.wantActions) {
				t.Fatalf("actions = %+v, want %+v", got.Actions, tc.wantActions)
			}
			for i, want := range tc.wantActions {
				if got.Actions[i] != want {
					t.Errorf("action %d = %+v, want %+v", i, got.Actions[i], want)
				}
			}
			if got.Unchanged != tc.wantUnchanged {
				t.Errorf("unchanged = %d, want %d", got.Unchanged, tc.wantUnchanged)
			}
			joined := strings.Join(got.Warnings, " | ")
			for _, want := range tc.wantWarnings {
				if !strings.Contains(joined, want) {
					t.Errorf("warnings %q should contain %q", joined, want)
				}
			}
			if tc.wantUnmanaged == nil {
				if len(got.Unmanaged) != 0 {
					t.Errorf("unmanaged = %v, want none", got.Unmanaged)
				}
			} else if !slices.Equal(got.Unmanaged, tc.wantUnmanaged) {
				t.Errorf("unmanaged = %v, want %v", got.Unmanaged, tc.wantUnmanaged)
			}
		})
	}
}

// Nothing in the plan may depend on the order Zone happens to return records in, or on the
// order of a map: both vary between runs, and a plan that varies is a plan that cannot be
// reasoned about when it goes wrong at 3am.
func TestPlanIgnoresListingAndMapOrder(t *testing.T) {
	desired := []string{"a.example.org", "b.example.org", "c.other.org"}
	listings := map[string][]Record{
		"example.org": {rec("1", "a.example.org", "203.0.113.1"), rec("2", "b.example.org", cycleIP)},
		"other.org":   {rec("3", "c.other.org", "203.0.113.5")},
	}

	want := plan(desired, listings, cycleIP, PlanOptions{})
	for i := 0; i < 25; i++ {
		shuffled := map[string][]Record{
			"other.org":   append([]Record(nil), listings["other.org"]...),
			"example.org": append([]Record(nil), listings["example.org"]...),
		}
		slices.Reverse(shuffled["example.org"])
		got := plan(desired, shuffled, cycleIP, PlanOptions{})
		if len(got.Actions) != len(want.Actions) {
			t.Fatalf("iteration %d changed the number of actions: %v vs %v", i, got.Actions, want.Actions)
		}
		for j := range want.Actions {
			if got.Actions[j] != want.Actions[j] {
				t.Errorf("iteration %d: action %d = %+v, want %+v", i, j, got.Actions[j], want.Actions[j])
			}
		}
	}
}

// A name managed in one zone must not appear as unmanaged just because another managed
// name in the same zone was processed first - the obvious bug in the first draft of this.
func TestPlanDoesNotReportManagedNamesAsUnmanaged(t *testing.T) {
	got := plan([]string{"stackoverlord.dev", "saturn.stackoverlord.dev"},
		map[string][]Record{"stackoverlord.dev": {
			rec("1", "stackoverlord.dev", cycleIP),
			rec("2", "saturn.stackoverlord.dev", cycleIP),
		}}, cycleIP, PlanOptions{})

	if len(got.Unmanaged) != 0 {
		t.Errorf("both names are managed, but unmanaged = %v", got.Unmanaged)
	}
	if got.Unchanged != 2 {
		t.Errorf("unchanged = %d, want 2", got.Unchanged)
	}
}
