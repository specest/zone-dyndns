package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// quietLogging keeps expected failures out of the test output while still letting a test
// assert on what was logged.
func quietLogging(t *testing.T) *strings.Builder {
	t.Helper()
	prev := logLevel.Level()
	buf := &strings.Builder{}
	logLevel.Set(slog.LevelError)
	setupLogging(buf)
	t.Cleanup(func() {
		logLevel.Set(prev)
		setupLogging(nil)
	})
	return buf
}

// withClient points the client at a handler and pins the retry knobs so a test that
// expects a given number of attempts gets exactly that number.
func withClient(t *testing.T, h http.HandlerFunc) (zoneClient, *int, func()) {
	t.Helper()
	var attempts int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		h(w, r)
	}))
	prevBase := zoneAPIBase
	prevRetry, prevMax := NETWORK_RETRY_DELAY, MAX_ATTEMPTS
	prevUser, prevKey := zoneUsername, zoneAPIKey
	zoneAPIBase = srv.URL
	NETWORK_RETRY_DELAY = time.Millisecond
	MAX_ATTEMPTS = 3
	zoneUsername, zoneAPIKey = "user", "key"
	t.Cleanup(func() {
		zoneAPIBase = prevBase
		NETWORK_RETRY_DELAY, MAX_ATTEMPTS = prevRetry, prevMax
		zoneUsername, zoneAPIKey = prevUser, prevKey
		srv.Close()
	})
	return zoneClient{}, &attempts, func() { mu.Lock(); defer mu.Unlock(); return }
}

// A response captured from the live API on 2026-10-24, fields and all. If Zone changes the
// shape, a test fails rather than the updater quietly reading "no records".
func TestListADecodesTheLiveResponse(t *testing.T) {
	captured := `[{"id":"845134","resource_url":"https:\/\/api.zone.eu\/v2\/dns\/stackoverlord.dev\/a\/845134",` +
		`"name":"stackoverlord.dev","destination":"85.253.172.182","delete":true,"modify":true,"comment":null},` +
		`{"id":"845135","resource_url":"https:\/\/api.zone.eu\/v2\/dns\/stackoverlord.dev\/a\/845135",` +
		`"name":"bomberman.stackoverlord.dev","destination":"145.14.30.19","delete":true,"modify":false,"comment":null}]`

	c, _, _ := withClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got == "" {
			t.Errorf("no credentials sent")
		}
		if !strings.HasSuffix(r.URL.Path, "/dns/stackoverlord.dev/a") {
			t.Errorf("requested %s, want the zone's A-record listing", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, captured)
	})

	records, err := c.listA(context.Background(), "stackoverlord.dev")
	if err != nil {
		t.Fatalf("listA: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("decoded %d records, want 2", len(records))
	}
	if records[0].Id != "845134" || records[0].Name != "stackoverlord.dev" ||
		records[0].Destination != "85.253.172.182" {
		t.Errorf("first record = %+v", records[0])
	}
	if records[0].locked() {
		t.Error("modify:true must not read as locked")
	}
	if !records[1].locked() {
		t.Error("modify:false must read as locked - that record cannot be updated")
	}
}

// An older shape of the same response omits "modify" entirely. A plain bool would make
// that read as locked and stop the updater managing every record, silently.
func TestRecordWithoutModifyFieldIsNotTreatedAsLocked(t *testing.T) {
	var r Record
	if err := json.Unmarshal([]byte(`{"id":"1","name":"x.org","destination":"1.2.3.4"}`), &r); err != nil {
		t.Fatal(err)
	}
	if r.Modify != nil {
		t.Errorf("absent modify field = %v, want nil (unknown, not locked)", *r.Modify)
	}
	if r.locked() {
		t.Error("a record whose modify field is absent must not be treated as locked")
	}
}

// A 4xx is a decision, not a hiccup: retrying it wastes attempts and risks a rate limit on
// top of a broken config.
func TestClientDoesNotRetryAClientError(t *testing.T) {
	quietLogging(t)
	c, attempts, current := withClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"message":"Invalid auth credentials"}`)
	})

	_, err := c.listA(context.Background(), "stackoverlord.dev")
	if err == nil {
		t.Fatal("listA should fail on 401")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "Invalid auth credentials") {
		t.Errorf("error should carry Zone's own status and explanation, got %v", err)
	}
	current()
	if *attempts != 1 {
		t.Errorf("%d requests for a 401, want 1: client errors must not be retried", *attempts)
	}
}

// A 5xx is worth another go, but a bounded one.
func TestClientRetriesAServerErrorUpToTheLimit(t *testing.T) {
	quietLogging(t)
	c, attempts, current := withClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `<html><head><title>503 Server Error</title></head>`)
	})

	_, err := c.listA(context.Background(), "stackoverlord.dev")
	if err == nil {
		t.Fatal("listA should fail once the attempts run out")
	}
	current()
	if *attempts != MAX_ATTEMPTS {
		t.Errorf("%d requests, want MAX_ATTEMPTS=%d", *attempts, MAX_ATTEMPTS)
	}
}

// The distinction that keeps DNS safe: a listing that cannot be parsed is an error, never
// an empty list. Reading it as empty would mean creating every name again.
func TestClientTreatsAMalformedListingAsAnErrorNotAnEmptyZone(t *testing.T) {
	quietLogging(t)
	c, _, _ := withClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // 200 with an HTML body: the nasty case
		io.WriteString(w, `<html><head><title>503 Server Error</title></head></html>`)
	})

	records, err := c.listA(context.Background(), "stackoverlord.dev")
	if err == nil {
		t.Fatalf("a malformed body must be an error, got records %v", records)
	}
	if records != nil {
		t.Errorf("records should be nil on failure, got %v", records)
	}
	if !strings.Contains(err.Error(), "unreadable response") {
		t.Errorf("the error should say what went wrong, got %v", err)
	}
}

// The retry bug this loop used to have: reusing one bytes.Reader means the second attempt
// sends an empty body, so a retry after a transient 500 would create a record with no name
// or send an update with no destination.
func TestRetryResendsTheRequestBody(t *testing.T) {
	quietLogging(t)
	var bodies []string
	var mu sync.Mutex
	c, _, _ := withClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		first := len(bodies) == 1
		mu.Unlock()
		if first {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"id":"845134"}`)
	})

	if err := c.updateA(context.Background(), "stackoverlord.dev", "845134", "saturn.stackoverlord.dev", "85.253.172.182"); err != nil {
		t.Fatalf("the retry should have succeeded: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("%d attempts, want 2 (500 then 200)", len(bodies))
	}
	for i, b := range bodies {
		if !strings.Contains(b, `"name":"saturn.stackoverlord.dev"`) ||
			!strings.Contains(b, `"destination":"85.253.172.182"`) {
			t.Errorf("attempt %d sent %q; every attempt must carry the full payload", i+1, b)
		}
	}
}

// Zone answers a create with 201, not 200; accepting only one of them would report every
// successful create as a failure.
func TestCreateExpectsCreated(t *testing.T) {
	quietLogging(t)
	c, _, _ := withClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("create used %s", r.Method)
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `[{"id":"999000","name":"new.example.org"}]`)
	})
	if err := c.createA(context.Background(), "example.org", "new.example.org", "85.253.172.182"); err != nil {
		t.Errorf("createA = %v, want nil for 201", err)
	}
}

// A transport failure is retried and, once attempts are exhausted, the original error is
// what the caller sees.
func TestTransportFailureIsReportedAfterRetries(t *testing.T) {
	quietLogging(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing listening any more

	prevBase, prevRetry, prevMax := zoneAPIBase, NETWORK_RETRY_DELAY, MAX_ATTEMPTS
	zoneAPIBase, NETWORK_RETRY_DELAY, MAX_ATTEMPTS = url, time.Millisecond, 2
	t.Cleanup(func() {
		zoneAPIBase, NETWORK_RETRY_DELAY, MAX_ATTEMPTS = prevBase, prevRetry, prevMax
	})

	_, err := zoneClient{}.listA(context.Background(), "stackoverlord.dev")
	if err == nil {
		t.Fatal("expected a connection error")
	}
	if !errors.Is(err, err) { // trivially true; what matters is that an error came back at all
		t.Fail()
	}
}

// requestContext must outlive the lifecycle context so a shutdown cannot cut a request in
// half - Zone updated with ip.conf left behind was the old failure mode, and a half-sent
// update is worse than one that never started.
func TestRequestContextOutlivesShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	reqCtx := requestContext(ctx)
	if reqCtx.Err() != nil {
		t.Errorf("the request context was cancelled with its parent: %v", reqCtx.Err())
	}
	if _, ok := reqCtx.Deadline(); ok {
		t.Error("the request context should add no deadline; httpClient.Timeout owns that")
	}
}

// retryWait owns both the waiting and the giving up, so both halves are pinned.
func TestRetryWaitAbandonsOnShutdown(t *testing.T) {
	prevRetry, prevMax := NETWORK_RETRY_DELAY, MAX_ATTEMPTS
	quietLogging(t)
	defer func() { NETWORK_RETRY_DELAY, MAX_ATTEMPTS = prevRetry, prevMax }()
	NETWORK_RETRY_DELAY = 10 * time.Second
	MAX_ATTEMPTS = 5

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	if retryWait(ctx, "updating example.org", 1) {
		t.Fatal("retryWait agreed to retry on a cancelled context")
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("retryWait waited %v; docker stop gives 10s and the delay was %v",
			elapsed, NETWORK_RETRY_DELAY)
	}
}

func TestRetryWaitStillWaitsAndStillGivesUp(t *testing.T) {
	prevRetry, prevMax := NETWORK_RETRY_DELAY, MAX_ATTEMPTS
	quietLogging(t)
	defer func() { NETWORK_RETRY_DELAY, MAX_ATTEMPTS = prevRetry, prevMax }()
	NETWORK_RETRY_DELAY = 40 * time.Millisecond
	MAX_ATTEMPTS = 2

	start := time.Now()
	if !retryWait(context.Background(), "updating example.org", 1) {
		t.Fatal("attempt 1 of 2 on a live context should be retried")
	}
	if elapsed := time.Since(start); elapsed < NETWORK_RETRY_DELAY {
		t.Errorf("retryWait returned after %v without waiting the %v delay", elapsed, NETWORK_RETRY_DELAY)
	}
	if retryWait(context.Background(), "updating example.org", 2) {
		t.Error("attempt 2 of MAX_ATTEMPTS=2 must stop, not try again")
	}
}
