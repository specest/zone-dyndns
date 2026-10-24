package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// zoneStub stands in for api.zone.eu: it answers per-zone listings and counts what it was
// asked to mutate. Assertions about "no request was sent" matter more here than anywhere
// else in the suite, because the dangerous outcome of a bug is a write, not a wrong read.
type zoneStub struct {
	mu sync.Mutex

	// listPerZone maps a zone root to the JSON body returned for GET /dns/<zone>/a.
	listPerZone map[string]string
	// listStatus forces a failure for one zone's listing.
	listStatus map[string]int
	// mutateStatus forces a failure for PUT and POST.
	mutateStatus int
	// afterMutate runs once a mutating request has been recorded. It exists so a test can
	// make a shutdown signal land in the middle of a cycle rather than before it.
	afterMutate func()

	requests   []string
	putBodies  []string
	postBodies []string
}

func (s *zoneStub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.requests = append(s.requests, r.Method+" "+r.URL.Path)

		switch r.Method {
		case http.MethodGet:
			zone := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/dns/"), "/a")
			if st := s.listStatus[zone]; st != 0 {
				w.WriteHeader(st)
				fmt.Fprint(w, `{"result":"error","message":"Teenust ei leitud"}`)
				return
			}
			body, ok := s.listPerZone[zone]
			if !ok {
				body = `[]`
			}
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, body)

		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			s.putBodies = append(s.putBodies, string(b))
			if s.afterMutate != nil {
				s.afterMutate()
			}
			if s.mutateStatus != 0 && s.mutateStatus != http.StatusOK {
				w.WriteHeader(s.mutateStatus)
				fmt.Fprint(w, `{"message":"Invalid auth credentials"}`)
				return
			}
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"id":"845134"}`)

		case http.MethodPost:
			b, _ := io.ReadAll(r.Body)
			s.postBodies = append(s.postBodies, string(b))
			if s.afterMutate != nil {
				s.afterMutate()
			}
			code := s.mutateStatus
			if code == 0 {
				code = http.StatusCreated
			}
			w.WriteHeader(code)
			fmt.Fprint(w, `[]`)
		}
	}
}

func (s *zoneStub) count(prefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.requests {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

func (s *zoneStub) mutations() int { return s.count("PUT ") + s.count("POST ") }

func (s *zoneStub) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

// fixture is the world one cycle needs: stand-in servers, a temporary conf directory as
// the working directory, and the log in a buffer. Every global it changes is restored,
// because these tests share one process and a leaked zoneAPIBase would send a later test
// to the real API.
type fixture struct {
	stub      *zoneStub
	logs      *bytes.Buffer
	ipLookups *atomic.Int64
}

func (f *fixture) logLines() []string {
	out := strings.TrimRight(f.logs.String(), "\n")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// recordsPath is the config file inside the fixture's working directory.
func recordsPath() string { return filepath.Join("conf", "records.conf") }

func newFixture(t *testing.T, records string, stub *zoneStub) *fixture {
	t.Helper()

	prevAPI, prevIPURL := zoneAPIBase, ipServiceURL
	prevUser, prevKey := zoneUsername, zoneAPIKey
	prevRetry, prevFreq, prevMax := NETWORK_RETRY_DELAY, CHECK_FREQUENCY, MAX_ATTEMPTS
	prevLevel := logLevel.Level()
	prevDry, prevEmpty := dryRun, allowEmptyZone
	dryRun, allowEmptyZone = false, false

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "conf"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "conf", "records.conf"), []byte(records), 0o660); err != nil {
		t.Fatal(err)
	}

	api := httptest.NewServer(stub.handler())
	ipLookups := &atomic.Int64{}
	ipSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ipLookups.Add(1)
		fmt.Fprint(w, "85.253.172.182\n")
	}))

	zoneAPIBase, ipServiceURL = api.URL, ipSrv.URL
	zoneUsername, zoneAPIKey = "user", "key"
	NETWORK_RETRY_DELAY = 10 * time.Millisecond
	CHECK_FREQUENCY = time.Hour // runOnce never sleeps; this only keeps it plausible
	MAX_ATTEMPTS = 2

	buf := &bytes.Buffer{}
	logLevel.Set(slog.LevelInfo)
	setupLogging(buf)
	t.Chdir(dir)

	f := &fixture{stub: stub, logs: buf, ipLookups: ipLookups}
	t.Cleanup(func() {
		zoneAPIBase, ipServiceURL = prevAPI, prevIPURL
		zoneUsername, zoneAPIKey = prevUser, prevKey
		NETWORK_RETRY_DELAY, CHECK_FREQUENCY, MAX_ATTEMPTS = prevRetry, prevFreq, prevMax
		dryRun, allowEmptyZone = prevDry, prevEmpty
		logLevel.Set(prevLevel)
		setupLogging(nil)
		api.Close()
		ipSrv.Close()
	})
	return f
}

func record(id, name, dest string) string {
	return fmt.Sprintf(`{"id":"%s","name":"%s","destination":"%s","modify":true}`, id, name, dest)
}

func listing(records ...string) string {
	return "[" + strings.Join(records, ",") + "]"
}

func readRecords(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(recordsPath())
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// A stale record is repointed using the id from the live listing. Nothing on disk holds
// an id any more, so nothing on disk can be out of date.
func TestRunOnceRepointsStaleRecordsFromTheListing(t *testing.T) {
	f := newFixture(t, "# mine\nstackoverlord.dev=845134\nsaturn.stackoverlord.dev=856494\n",
		&zoneStub{listPerZone: map[string]string{
			"stackoverlord.dev": listing(
				record("845134", "stackoverlord.dev", "203.0.113.1"),
				record("856494", "saturn.stackoverlord.dev", "203.0.113.1"),
			),
		}})

	if !runOnce(context.Background()) {
		t.Fatal("runOnce reported an unusable config")
	}
	// Both names are stale, so both are repointed - but only one listing was needed, and
	// each PUT carries the id Zone reported rather than the number in records.conf.
	if got := f.stub.count("PUT "); got != 2 {
		t.Errorf("%d PUT requests, want 2 (%v)", got, f.stub.all())
	}
	if got := f.stub.count("GET "); got != 1 {
		t.Errorf("%d listings, want 1: both names live in the same zone", got)
	}
	for _, body := range f.stub.putBodies {
		if !strings.Contains(body, `"destination":"85.253.172.182"`) {
			t.Errorf("PUT payload wrong: %s", body)
		}
	}
	// Aimed at the ids the listing reported. Which ones is beside the point: records.conf
	// holds none any more, so it can no longer send a stale one.
	if !slices.Contains(f.stub.all(), "PUT /dns/stackoverlord.dev/a/845134") {
		t.Errorf("expected an update aimed at the id from the listing, got %v", f.stub.all())
	}
	if !strings.Contains(f.logs.String(), `msg="record updated"`) {
		t.Errorf("the update should be reported: %v", f.logLines())
	}
	// The legacy id suffix is dropped from the file, and the comment survives.
	got := readRecords(t)
	if !strings.Contains(got, "# mine\n") || !strings.Contains(got, "\nstackoverlord.dev\n") {
		t.Errorf("records.conf was not canonicalised to bare names while keeping comments:\n%s", got)
	}
	if strings.Contains(got, "=845134") {
		t.Errorf("the obsolete id is still in the file:\n%s", got)
	}
}

// A name with no record at all is created - but only because the listing was readable and
// non-empty, which is what distinguishes this from the next two cases.
func TestRunOnceCreatesAMissingRecord(t *testing.T) {
	f := newFixture(t, "new.example.org\n", &zoneStub{listPerZone: map[string]string{
		"example.org": listing(record("1", "other.example.org", "203.0.113.9")),
	}})

	if !runOnce(context.Background()) {
		t.Fatal("runOnce reported an unusable config")
	}
	if got := f.stub.count("POST "); got != 1 {
		t.Errorf("%d POST requests, want 1 (%v)", got, f.stub.all())
	}
	if len(f.stub.postBodies) == 1 &&
		!strings.Contains(f.stub.postBodies[0], `"name":"new.example.org"`) {
		t.Errorf("create payload wrong: %s", f.stub.postBodies[0])
	}
	if !strings.Contains(f.logs.String(), `msg="record created"`) {
		t.Errorf("creation should be reported: %v", f.logLines())
	}
}

// The case the whole design is built around: a listing that could not be read must not be
// read as "no records exist". With four unmanaged A-records in a real zone of this
// project, mistaking those two things duplicates live services.
func TestRunOnceDoesNotCreateWhenTheListingFailed(t *testing.T) {
	f := newFixture(t, "saturn.stackoverlord.dev\n", &zoneStub{
		listStatus: map[string]int{"stackoverlord.dev": http.StatusInternalServerError},
	})

	if !runOnce(context.Background()) {
		t.Fatal("runOnce reported an unusable config")
	}
	if got := f.stub.mutations(); got != 0 {
		t.Errorf("%d mutations after a failed listing, want 0 (%v)", got, f.stub.all())
	}
	out := f.logs.String()
	if !strings.Contains(out, "leaving it alone this cycle") {
		t.Errorf("the skipped zone should be named: %v", f.logLines())
	}
	if !strings.Contains(out, "no listing available") {
		t.Errorf("the plan should also say it changed nothing: %v", f.logLines())
	}
}

// An empty listing is the one response that cannot be told apart from a lie, so creating
// is refused unless the operator says the zone really is new.
func TestRunOnceRefusesToCreateInAnEmptyZone(t *testing.T) {
	f := newFixture(t, "first.example.org\n", &zoneStub{
		listPerZone: map[string]string{"example.org": `[]`},
	})

	if !runOnce(context.Background()) {
		t.Fatal("runOnce reported an unusable config")
	}
	if got := f.stub.count("POST "); got != 0 {
		t.Errorf("%d creates into an empty listing, want 0 until -allow-empty-zone is given", got)
	}
	if !strings.Contains(f.logs.String(), "-allow-empty-zone") {
		t.Errorf("the log should name the way out: %v", f.logLines())
	}

	// Same world, flag on: the create goes ahead.
	allowEmptyZone = true
	f.logs.Reset()
	if !runOnce(context.Background()) {
		t.Fatal("runOnce reported an unusable config")
	}
	if got := f.stub.count("POST "); got != 1 {
		t.Errorf("%d creates with -allow-empty-zone, want 1 (%v)", got, f.stub.all())
	}
}

// Steady state at the default level: no requests, no log lines. This is what an idle
// container emits every CHECK_FREQUENCY.
func TestRunOnceIsSilentWhenEverythingAlreadyMatches(t *testing.T) {
	f := newFixture(t, "stackoverlord.dev\nsaturn.stackoverlord.dev\n", &zoneStub{
		listPerZone: map[string]string{
			"stackoverlord.dev": listing(
				record("845134", "stackoverlord.dev", "85.253.172.182"),
				record("856494", "saturn.stackoverlord.dev", "85.253.172.182"),
			),
		}})

	if !runOnce(context.Background()) {
		t.Fatal("runOnce reported an unusable config")
	}
	if lines := f.logLines(); lines != nil {
		t.Errorf("an unchanged cycle logged %d line(s) at info, want 0: %v", len(lines), lines)
	}
	if got := f.stub.mutations(); got != 0 {
		t.Errorf("%d mutations when nothing differed, want 0 (%v)", got, f.stub.all())
	}
	if got := f.stub.count("GET "); got != 1 {
		t.Errorf("%d listings, want 1", got)
	}
}

// The listing of a real zone contains records belonging to other machines. They must be
// neither changed nor deleted, and saying so at debug level is what turns "it ignored my
// other records" from an assumption into a statement.
func TestRunOnceLeavesUnmanagedRecordsAlone(t *testing.T) {
	f := newFixture(t, "stackoverlord.dev\n", &zoneStub{
		listPerZone: map[string]string{
			"stackoverlord.dev": listing(
				record("845134", "stackoverlord.dev", "203.0.113.1"),
				record("845135", "bomberman.stackoverlord.dev", "145.14.30.19"),
				record("845136", "sql.stackoverlord.dev", "145.14.30.19"),
				record("845175", "test.stackoverlord.dev", "94.246.238.120"),
				record("849611", "failid.stackoverlord.dev", "145.14.30.19"),
			),
		}})

	if !runOnce(context.Background()) {
		t.Fatal("runOnce reported an unusable config")
	}
	if got := f.stub.count("POST "); got != 0 {
		t.Errorf("%d creates, want 0 - unmanaged names must not be recreated (%v)", got, f.stub.all())
	}
	if got := f.stub.count("PUT "); got != 1 {
		t.Errorf("%d updates, want exactly 1 (the managed name) (%v)", got, f.stub.all())
	}
	logLevel.Set(slog.LevelDebug)
	f.logs.Reset()
	runOnce(context.Background())
	if !strings.Contains(f.logs.String(), "leaving unmanaged records alone") {
		t.Errorf("unmanaged records should be named at debug level: %v", f.logLines())
	}
}

// -dry-run exists so a change to this tool's judgement can be inspected against a real
// zone before it is allowed to write to one.
func TestRunOnceDryRunSendsNoMutations(t *testing.T) {
	// Written in the legacy form on purpose: a dry run must not canonicalise it either.
	f := newFixture(t, "stackoverlord.dev=845134\n", &zoneStub{
		listPerZone: map[string]string{
			"stackoverlord.dev": listing(record("845134", "stackoverlord.dev", "203.0.113.1")),
		}})
	dryRun = true

	if !runOnce(context.Background()) {
		t.Fatal("runOnce reported an unusable config")
	}
	if got := f.stub.mutations(); got != 0 {
		t.Errorf("%d mutations in a dry run, want 0 (%v)", got, f.stub.all())
	}
	out := f.logs.String()
	if !strings.Contains(out, `msg="dry run: no request sent"`) ||
		!strings.Contains(out, "action=update") || !strings.Contains(out, "would_set=85.253.172.182") {
		t.Errorf("the dry run should say exactly what it would have done: %v", f.logLines())
	}
	// "No writes" has to include the config file: canonicalising it during a dry run would
	// change the operator's file while claiming not to touch anything.
	if data, err := os.ReadFile(recordsPath()); err != nil {
		t.Fatal(err)
	} else if !strings.Contains(string(data), "stackoverlord.dev=845134") {
		t.Errorf("dry run rewrote records.conf:\n%s", data)
	}
}

// A signal before the cycle starts must stop it reaching DNS, while the lookups already
// under way are allowed to finish.
func TestRunOnceHonoursShutdown(t *testing.T) {
	f := newFixture(t, "a.example.org\nb.example.org\nc.example.org\n", &zoneStub{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	if !runOnce(ctx) {
		t.Error("the config was usable, so runOnce should say so")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("runOnce took %v after shutdown; it should neither sleep nor retry", elapsed)
	}
	if got := f.stub.count(""); got != 0 {
		t.Errorf("no zone request should start after shutdown, got %v", f.stub.all())
	}
	if f.ipLookups.Load() != 1 {
		t.Errorf("%d IP lookups, want 1: the lookup already under way is allowed to finish",
			f.ipLookups.Load())
	}
	if strings.Contains(f.logs.String(), "level=ERROR") {
		t.Errorf("an interrupted cycle is not a failure: %v", f.logLines())
	}
}

// A signal that arrives mid-cycle has to stop the records that have not started yet. The
// one in flight is left alone on purpose - being cut in half is how Zone ends up updated
// while local state still describes the old address - so "no new work" is the whole deal.
func TestRunOnceStopsStartingWorkAfterAMidCycleSignal(t *testing.T) {
	stub := &zoneStub{listPerZone: map[string]string{
		"stackoverlord.dev": listing(
			record("845134", "a.stackoverlord.dev", "203.0.113.1"),
			record("845135", "b.stackoverlord.dev", "203.0.113.1"),
			record("845136", "c.stackoverlord.dev", "203.0.113.1"),
		),
	}}
	f := newFixture(t, "a.stackoverlord.dev\nb.stackoverlord.dev\nc.stackoverlord.dev\n", stub)

	ctx, cancel := context.WithCancel(context.Background())
	// The signal lands while the first update is being served: two names remain, and
	// neither may be sent.
	stub.afterMutate = cancel

	if !runOnce(ctx) {
		t.Error("the config was usable, so runOnce should say so")
	}
	if got := stub.mutations(); got != 1 {
		t.Errorf("%d mutations, want exactly 1 - the rest of the cycle must not start (%v)", got, stub.all())
	}
	if !strings.Contains(f.logs.String(), "leaving the rest of this cycle") {
		t.Errorf("the skipped work should be announced: %v", f.logLines())
	}
	// The record that was mid-flight still completed: shutting down is not a rollback.
	if !strings.Contains(f.logs.String(), `msg="record updated"`) {
		t.Errorf("the in-flight update should be reported as done: %v", f.logLines())
	}
}

func TestRunOnceReportsAnUnusableConfig(t *testing.T) {
	f := newFixture(t, "# mine\nlocalhost\n", &zoneStub{})

	if runOnce(context.Background()) {
		t.Error("runOnce should report the config as unusable")
	}
	if !strings.Contains(f.logs.String(), "config problem") {
		t.Errorf("the rejected line should be named, got %v", f.logLines())
	}
	if got := f.stub.count(""); got != 0 {
		t.Errorf("nothing should be requested for a config with nothing usable in it, got %v", f.stub.all())
	}
	if f.ipLookups.Load() != 0 {
		t.Error("the IP service should not be queried when there is nothing to publish")
	}
}

// A rejected address from the echo service must never become a record destination - the
// 2026-08-18 incident in updater.log was exactly this.
func TestRunOnceSkipsWhenTheIPAddressIsUnusable(t *testing.T) {
	f := newFixture(t, "stackoverlord.dev\n", &zoneStub{listPerZone: map[string]string{
		"stackoverlord.dev": listing(record("1", "stackoverlord.dev", "203.0.113.1")),
	}})

	// Point the IP service at something that answers with an error page but 200 OK.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html><head><title>503</title></head></html>")
	}))
	defer bad.Close()
	ipServiceURL = bad.URL
	MAX_ATTEMPTS = 1

	if !runOnce(context.Background()) {
		t.Error("an unusable address skips the cycle, it does not make the config unusable")
	}
	if got := f.stub.count(""); got != 0 {
		t.Errorf("no zone request should follow a bad address, got %v", f.stub.all())
	}
	if !strings.Contains(f.logs.String(), "not a routable IPv4 address") {
		t.Errorf("the rejected body should be reported: %v", f.logLines())
	}
}

// records.conf is the human's file; a directory that refuses writes must be a warning,
// not a cycle skipped - DNS is what this program is for.
func TestRunOnceStillUpdatesDNSWhenRecordsConfIsReadOnly(t *testing.T) {
	f := newFixture(t, "stackoverlord.dev\n", &zoneStub{
		listPerZone: map[string]string{
			"stackoverlord.dev": listing(record("845134", "stackoverlord.dev", "203.0.113.1")),
		}})
	if err := os.Chmod(filepath.Join("conf"), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join("conf"), 0o755) })

	if !runOnce(context.Background()) {
		t.Fatal("runOnce reported an unusable config")
	}
	if got := f.stub.count("PUT "); got != 1 {
		t.Errorf("%d updates, want 1 - a config that cannot be tidied must not stop DNS work (%v)",
			got, f.stub.all())
	}
	if !strings.Contains(f.logs.String(), "could not tidy the domain list locally") {
		t.Errorf("the failed tidy-up should be reported: %v", f.logLines())
	}
}

// The plan is a map keyed by zone; without sorting, which zone is listed first would vary
// between runs and make log output unorderable.
func TestZonesNeededSortsAndDeduplicates(t *testing.T) {
	zones, warnings := zonesNeeded([]string{
		"saturn.stackoverlord.dev", "stackoverlord.dev", "noorusefestival.ee", "localhost",
	})
	if !slices.Equal(zones, []string{"noorusefestival.ee", "stackoverlord.dev"}) {
		t.Errorf("zones = %v, want sorted and de-duplicated", zones)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "localhost") {
		t.Errorf("want one warning naming localhost, got %v", warnings)
	}
}
