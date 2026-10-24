package main

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

// ActionKind is what reconciliation decided to do about one (sub)domain.
type ActionKind string

const (
	actionCreate ActionKind = "create"
	actionUpdate ActionKind = "update"
)

// Action is one intended mutation of DNS.
type Action struct {
	Kind     ActionKind
	Domain   string
	Zone     string
	RecordID RecordID // updates only: always taken from the live listing, never from disk
	Current  string   // the destination DNS holds right now, for the log line
}

// Plan is the difference between what records.conf asks for and what Zone actually has.
type Plan struct {
	Actions []Action

	// Warnings are things a human should hear about: a zone we could not read, a name with
	// several A-records, a record Zone refuses to let us change, an empty listing we
	// declined to trust. They are not failures - the cycle may have done everything it
	// safely could.
	Warnings []string

	// Unchanged counts names already pointing where they should, which is the normal
	// answer and the reason a steady cycle sends no requests at all.
	Unchanged int

	// Unmanaged lists A-records in the zones we read that records.conf does not name. They
	// are reported and left exactly where they are: this tool adds records and repoints
	// them, it never deletes one.
	Unmanaged []string
}

// PlanOptions carries operator choices. Passing them in, rather than reading globals, is
// what keeps plan() pure enough that a table of cases can cover every branch.
type PlanOptions struct {
	// AllowEmptyZone permits creating the first record in a zone whose listing came back
	// with nothing in it. Without it an empty listing is refused, because "this zone is
	// brand new" and "the API gave us an empty answer it did not mean" are indistinguishable
	// from here, and guessing wrong duplicates every name in the file.
	AllowEmptyZone bool
}

// zonesNeeded returns the sorted, de-duplicated zone roots of the given names, plus a
// warning for each name that has none. One listing per zone is enough to reconcile every
// name in it, which is fewer requests than the old per-domain lookup.
func zonesNeeded(desired []string) ([]string, []string) {
	var zones, warnings []string
	seen := map[string]bool{}
	for _, name := range desired {
		zone, err := getDomainRoot(name)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		if !seen[zone] {
			seen[zone] = true
			zones = append(zones, zone)
		}
	}
	sort.Strings(zones)
	return zones, warnings
}

// plan compares the desired names against the per-zone listings.
//
// This is the entire policy of the tool as a pure function: no network, no clock, no
// logging. Everything dangerous about a dynamic DNS updater - when to create, when to
// overwrite, and above all when to do neither - is decided here, where each branch is one
// row in a test table rather than a code path that only appears when the network misbehaves.
func plan(desired []string, listings map[string][]Record, ip string, opts PlanOptions) Plan {
	var p Plan

	wanted := make(map[string]bool, len(desired))
	for _, n := range desired {
		wanted[strings.ToLower(n)] = true
	}

	for _, name := range desired {
		zone, err := getDomainRoot(name)
		if err != nil {
			// parseDomains normally rejects these; anything that gets through is reported
			// rather than acted on.
			p.Warnings = append(p.Warnings, fmt.Sprintf("%s: %v", name, err))
			continue
		}

		records, fetched := listings[zone]
		if !fetched {
			// No listing means we cannot tell create from update, and both guesses can be
			// wrong in ways that break DNS. The safe action is none; the next cycle tries
			// again with a fresh listing.
			p.Warnings = append(p.Warnings, fmt.Sprintf(
				"%s: no listing available for zone %s, nothing was changed", name, zone))
			continue
		}

		if len(records) == 0 && !opts.AllowEmptyZone {
			p.Warnings = append(p.Warnings, fmt.Sprintf(
				"%s: zone %s lists no A-records at all, refusing to create in case the listing is wrong "+
					"(use -allow-empty-zone if this zone really is new)", name, zone))
			continue
		}

		var matches []Record
		for _, r := range records {
			// DNS names are case-insensitive; comparing without folding would create a
			// second record for a name that exists in another casing.
			if strings.EqualFold(r.Name, name) {
				matches = append(matches, r)
			}
		}

		switch {
		case len(matches) == 0:
			p.Actions = append(p.Actions, Action{Kind: actionCreate, Domain: name, Zone: zone})

		case len(matches) > 1:
			// Several A-records for one name is valid DNS (round-robin) and this tool
			// cannot manage it: choosing one each cycle makes the published answer depend
			// on listing order, and choosing both is not something an A-record is for.
			ids := make([]string, 0, len(matches))
			for _, r := range matches {
				ids = append(ids, string(r.Id))
			}
			sort.Strings(ids)
			p.Warnings = append(p.Warnings, fmt.Sprintf(
				"%s has %d A-records (%s) and is left alone - keep just the one you want managed here",
				name, len(matches), strings.Join(ids, ", ")))

		case matches[0].locked():
			p.Warnings = append(p.Warnings, fmt.Sprintf(
				"%s: Zone reports record %s as not modifiable, skipping", name, matches[0].Id))

		case matches[0].Destination == ip:
			p.Unchanged++

		default:
			p.Actions = append(p.Actions, Action{
				Kind: actionUpdate, Domain: name, Zone: zone,
				RecordID: matches[0].Id, Current: matches[0].Destination,
			})
		}
	}

	for _, records := range listings {
		for _, r := range records {
			if !wanted[strings.ToLower(r.Name)] {
				p.Unmanaged = append(p.Unmanaged, r.Name)
			}
		}
	}
	sort.Strings(p.Unmanaged)
	p.Unmanaged = dedupe(p.Unmanaged)

	return p
}

// Summary counts what a cycle actually did.
type Summary struct {
	Created, Updated, Failed int
}

// changed reports whether anything happened worth announcing.
func (s Summary) changed() bool { return s.Created+s.Updated+s.Failed > 0 }

// apply carries out a plan. With dryRun it says what it would have done and sends no
// mutating request - which is how a change to this tool's judgement can be looked at
// against a real zone before it is allowed to write to it.
func apply(ctx context.Context, c zoneClient, p Plan, ip string, dryRun bool) Summary {
	var s Summary

	for _, a := range p.Actions {
		if ctx.Err() != nil {
			slog.Info("shutting down, leaving the rest of this cycle", "domain", a.Domain)
			break
		}

		if dryRun {
			slog.Info("dry run: no request sent", "action", string(a.Kind),
				"domain", a.Domain, "zone", a.Zone, "id", string(a.RecordID),
				"current", a.Current, "would_set", ip)
			continue
		}

		var err error
		switch a.Kind {
		case actionCreate:
			err = c.createA(ctx, a.Zone, a.Domain, ip)
			if err == nil {
				s.Created++
				slog.Info("record created", "domain", a.Domain, "zone", a.Zone, "destination", ip)
			}
		case actionUpdate:
			err = c.updateA(ctx, a.Zone, a.RecordID, a.Domain, ip)
			if err == nil {
				s.Updated++
				slog.Info("record updated", "domain", a.Domain, "zone", a.Zone,
					"destination", ip, "previous", a.Current)
			}
		}
		if err != nil {
			// The client already logged status, body and retries; counting is enough here.
			s.Failed++
		}
	}

	return s
}

// dedupe removes repeated entries from a sorted slice.
func dedupe(sorted []string) []string {
	out := sorted[:0]
	for i, v := range sorted {
		if i == 0 || v != sorted[i-1] {
			out = append(out, v)
		}
	}
	return out
}
