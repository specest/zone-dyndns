package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
)

// RecordID is a Zone DNS record identifier.
//
// The Zone OpenAPI spec types "id" as an integer while the live API returns it quoted as
// a string ({"id":"845134",...}). A plain string field does not fail cleanly when the
// type changes: Go's decoder is partial, so it fills in the rest of the struct and leaves
// the id empty, and an empty id means a record that can never be updated. Accept both
// spellings and keep the value as text, since it only ever goes into a URL.
type RecordID string

func (id *RecordID) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*id = RecordID(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err == nil {
		*id = RecordID(n.String())
		return nil
	}
	return fmt.Errorf("record id %s is neither a JSON string nor a JSON number", b)
}

// Record is one A-record as the Zone API returns it. Only the fields this tool acts on
// are modelled; anything else in the response is ignored by the decoder.
type Record struct {
	Id          RecordID `json:"id"`
	Name        string   `json:"name"`
	Destination string   `json:"destination"`

	// Modify is what Zone itself reports about whether a record may be changed, and it is
	// a pointer on purpose: responses exist that omit the field entirely, and with a plain
	// bool an absent field would read as "locked" - the updater would then quietly stop
	// managing every record. Absent means "unknown, go ahead and try".
	Modify *bool `json:"modify"`
}

// locked reports whether Zone explicitly refuses modification of this record.
func (r Record) locked() bool { return r.Modify != nil && !*r.Modify }

// zoneClient is the whole of this program's relationship with api.zone.eu: three calls,
// bounded retries, one log line per outcome.
type zoneClient struct{}

// listA returns every A-record in a zone.
//
// Zone ignores paging parameters - verified against the live API, where ?limit=1,
// ?per_page=1 and ?page=2 each returned the full list - so one request really is a
// complete picture. Reconciling depends on that: a truncated listing would look like
// "this record does not exist yet" and turn into a duplicate.
func (c zoneClient) listA(ctx context.Context, zone string) ([]Record, error) {
	var records []Record
	url := fmt.Sprintf("%s/dns/%s/a", zoneAPIBase, zone)
	err := c.do(ctx, http.MethodGet, url, nil, http.StatusOK, &records, "listing A-records for "+zone)
	return records, err
}

// updateA repoints an existing record; Zone answers 200 with the record.
func (c zoneClient) updateA(ctx context.Context, zone string, id RecordID, name, ip string) error {
	url := fmt.Sprintf("%s/dns/%s/a/%s", zoneAPIBase, zone, id)
	return c.do(ctx, http.MethodPut, url, recordPayload(name, ip), http.StatusOK, nil, "updating "+name)
}

// createA adds a record; Zone answers 201.
func (c zoneClient) createA(ctx context.Context, zone, name, ip string) error {
	url := fmt.Sprintf("%s/dns/%s/a", zoneAPIBase, zone)
	return c.do(ctx, http.MethodPost, url, recordPayload(name, ip), http.StatusCreated, nil, "creating "+name)
}

// recordPayload marshals rather than Sprintf-ing, so a value cannot break out of the JSON
// document it is being placed into.
func recordPayload(name, ip string) []byte {
	b, err := json.Marshal(map[string]string{"name": name, "destination": ip})
	if err != nil {
		// Unreachable for two strings. Returning nil rather than dying keeps the caller on
		// its error path: no payload means no request.
		slog.Error("cannot encode the request payload", "domain", name, "err", err)
		return nil
	}
	return b
}

// do issues one request, retrying transport failures and 5xx answers within the bounds
// retryWait sets, and returning an error for a rejection that retrying cannot fix.
//
// This loop replaced per-callsite recursion. The recursive versions had no attempt limit
// at all, and two paths retried with no delay, so one persistent failure became a busy
// loop against the API.
func (c zoneClient) do(ctx context.Context, method, url string, payload []byte, wantStatus int, out any, what string) error {
	for attempt := 1; ; attempt++ {
		// A fresh reader per attempt: a retried request has to resend its body, and a
		// bytes.Reader reused after the first attempt is exhausted and would send an
		// empty request - a bug that only shows up when the network is already misbehaving.
		var body io.Reader
		if payload != nil {
			body = bytes.NewReader(payload)
		}

		req, err := http.NewRequestWithContext(requestContext(ctx), method, url, body)
		if err != nil {
			slog.Error("cannot build the request", "op", what, "err", err)
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.SetBasicAuth(zoneUsername, zoneAPIKey)

		resp, err := httpClient.Do(req)
		if err != nil {
			slog.Warn("zone API request failed", "op", what, "err", err)
			if !retryWait(ctx, what, attempt) {
				return err
			}
			continue
		}

		data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		// Closed here rather than deferred: this is a loop, and deferring would hold every
		// attempt's body open until the function finally returns.
		resp.Body.Close()

		if readErr != nil {
			slog.Warn("failed to read the zone API response", "op", what, "err", readErr)
			if !retryWait(ctx, what, attempt) {
				return readErr
			}
			continue
		}

		if resp.StatusCode != wantStatus {
			if resp.StatusCode >= http.StatusInternalServerError {
				slog.Warn("zone API returned a server error", "op", what,
					"status", resp.Status, "body", brief(string(data), 200))
				if !retryWait(ctx, what, attempt) {
					return fmt.Errorf("%s: %s", what, resp.Status)
				}
				continue
			}
			// A 4xx is a decision, not a hiccup: 401 will still be 401 on the fifth
			// attempt, and hammering Zone with it invites a rate limit on top of a broken
			// config. Report the body - it is Zone's own explanation, and better than any
			// guess we could make about the cause.
			slog.Warn("zone API rejected the request", "op", what,
				"status", resp.Status, "body", brief(string(data), 200))
			return fmt.Errorf("%s: %s %s", what, resp.Status, brief(string(data), 200))
		}

		if out != nil {
			if err := json.Unmarshal(data, out); err != nil {
				// A malformed listing is never treated as an empty one. That distinction is
				// the difference between "do nothing this cycle" and "create a second
				// A-record for a name that already has one" - which is an outage for
				// whatever resolves that name.
				slog.Warn("could not parse the zone API response", "op", what,
					"err", err, "body", brief(string(data), 200))
				return fmt.Errorf("%s: unreadable response: %w", what, err)
			}
		}
		return nil
	}
}
