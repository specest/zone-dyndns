package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// TestIsRoutableIPv4 pins down the guard that stops a broken IP echo service from
// turning into a DNS update. The "invalid" entries are all things an IP service has
// been observed to return (a 503 error page, an empty body) or can return when it is
// confused (loopback, broadcast). Accepting any of them would point every configured
// A-record at an unreachable destination.
func TestIsRoutableIPv4(t *testing.T) {
	valid := []string{
		"85.253.172.88",
		"145.14.30.19",
		"10.0.0.1",    // LAN / VPN side is a legitimate record destination
		"192.168.1.5", //
		"172.16.0.9",  //
		"100.64.1.1",  // CGNAT / Tailscale range
	}
	invalid := []string{
		"",
		" ",
		"<html><head>",
		"503 Service Unavailable",
		"127.0.0.1",
		"0.0.0.0",
		"169.254.1.1",
		"224.0.0.5",
		"255.255.255.255",
		"2001:db8::1", // this tool only manages A records
		"::1",
		"85.253.172.88 ", // untrimmed input is not acceptable
		"85.253.172.88\n<html>",
		"not.an.ip",
		"85.253.172.256",
	}

	for _, s := range valid {
		if !isRoutableIPv4(s) {
			t.Errorf("isRoutableIPv4(%q) = false, want true", s)
		}
	}
	for _, s := range invalid {
		if isRoutableIPv4(s) {
			t.Errorf("isRoutableIPv4(%q) = true, want false", s)
		}
	}
}

// TestRecordIDAcceptsBothSpellings guards the mismatch between the Zone OpenAPI spec
// (id: integer) and the live API (id: a quoted string). Decoding a number into a plain
// string field does not fail cleanly - Go's decoder is partial and leaves the id empty,
// which would silently stop the updater from ever touching that record again.
func TestRecordIDAcceptsBothSpellings(t *testing.T) {
	cases := []struct {
		body string
		want RecordID
	}{
		{`{"id":"845134","name":"x"}`, "845134"}, // what the live API returns today
		{`{"id":845134,"name":"x"}`, "845134"},   // what the OpenAPI spec describes
		{`{"id":null,"name":"x"}`, ""},           // unusable: caught by the empty-id guard
		{`{"id":"","name":"x"}`, ""},             // bare json.Number errors on this one
		{`{"name":"x"}`, ""},                     // absent id
	}
	for _, c := range cases {
		var r Record
		if err := json.Unmarshal([]byte(c.body), &r); err != nil {
			t.Errorf("json.Unmarshal(%s) = %v, want no error", c.body, err)
			continue
		}
		if r.Id != c.want {
			t.Errorf("json.Unmarshal(%s) id = %q, want %q", c.body, r.Id, c.want)
		}
	}

	// Anything else must be a loud failure rather than an empty id.
	for _, body := range []string{`{"id":true}`, `{"id":{"a":1}}`, `{"id":[1]`} {
		var r Record
		if err := json.Unmarshal([]byte(body), &r); err == nil {
			t.Errorf("json.Unmarshal(%s) = nil error, want a failure naming the bad id", body)
		}
	}
}

// TestDecodeRecordList uses a response body captured verbatim from logs/updater.log
// (2026-10-04 16:40:23) so that a change in the shape of Zone's answer fails a test
// instead of quietly disabling DNS updates.
func TestDecodeRecordList(t *testing.T) {
	captured := []byte(`[{"id":"845134","resource_url":"https:\/\/api.zone.eu\/v2\/dns\/stackoverlord.dev\/a\/845134","name":"stackoverlord.dev","destination":"85.253.172.88"},{"id":"856494","resource_url":"https:\/\/api.zone.eu\/v2\/dns\/stackoverlord.dev\/a\/856494","name":"saturn.stackoverlord.dev","destination":"85.253.172.88"}]`)

	var r []Record
	if err := json.Unmarshal(captured, &r); err != nil {
		t.Fatalf("the captured live response must decode, got %v", err)
	}
	if len(r) != 2 {
		t.Fatalf("decoded %d records, want 2", len(r))
	}
	if r[0].Id != "845134" || r[0].Name != "stackoverlord.dev" {
		t.Errorf("first record = %+v, want id 845134 / stackoverlord.dev", r[0])
	}
	if r[1].Id != "856494" || r[1].Name != "saturn.stackoverlord.dev" {
		t.Errorf("second record = %+v, want id 856494 / saturn.stackoverlord.dev", r[1])
	}

	// The same zone with ids as bare numbers, per the OpenAPI spec, must decode the same.
	numeric := []byte(`[{"id":845134,"name":"stackoverlord.dev"},{"id":856494,"name":"saturn.stackoverlord.dev"}]`)
	var n []Record
	if err := json.Unmarshal(numeric, &n); err != nil {
		t.Fatalf("a spec-shaped response must decode, got %v", err)
	}
	if len(n) != 2 || n[0].Id != "845134" || n[1].Id != "856494" {
		t.Errorf("spec-shaped response decoded to %+v", n)
	}

	// An error page must fail loudly, not decode into an empty list that the caller
	// reads as "this record does not exist yet" and then tries to create.
	var bad []Record
	if err := json.Unmarshal([]byte("<html><head><title>503 Server Error</title></head>"), &bad); err == nil {
		t.Error("an HTML error page must not decode as a record list")
	}
}

// An error page used to be logged verbatim and arrived as nine separate lines, which is
// how the 2026-08-18 incident got buried in updater.log.
// The case that used to panic with "index out of range [-1]": one bare hostname in
// records.conf killed the daemon and restart:always crash-looped it.
func TestGetDomainRootRejectsSingleLabelInsteadOfPanicking(t *testing.T) {
	if got, err := getDomainRoot("localhost"); err == nil {
		t.Errorf("getDomainRoot(\"localhost\") = %q, want an error", got)
	}
	if got, err := getDomainRoot(""); err == nil {
		t.Errorf("getDomainRoot(\"\") = %q, want an error", got)
	}

	for _, c := range []struct{ in, want string }{
		{"stackoverlord.dev", "stackoverlord.dev"},
		{"saturn.stackoverlord.dev", "stackoverlord.dev"},
		{"a.b.example.ee", "example.ee"},
	} {
		got, err := getDomainRoot(c.in)
		if err != nil || got != c.want {
			t.Errorf("getDomainRoot(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}

	// Documented limitation, pinned here so a future change cannot alter it silently:
	// multi-part public suffixes resolve to the wrong zone.
	if got, _ := getDomainRoot("blog.example.co.uk"); got != "co.uk" {
		t.Errorf("multi-part suffix behaviour changed: got %q, want co.uk", got)
	}
}

func TestValidDomain(t *testing.T) {
	valid := []string{
		"example.org", "noorusefestival.ee", "saturn.stackoverlord.dev",
		"a-b.example.ee", "xn--kirju.ee", "x1.y2.z3.example.dev",
	}
	invalid := []string{
		"", "localhost", ".example.org", "example.org.", "ex ample.org",
		"example_org.org", "-example.org", "example-.org", "example..org",
		strings.Repeat("a", 64) + ".org", "例え.jp", "example.org ", "a b",
	}
	for _, d := range valid {
		if !validDomain(d) {
			t.Errorf("validDomain(%q) = false, want true", d)
		}
	}
	for _, d := range invalid {
		if validDomain(d) {
			t.Errorf("validDomain(%q) = true, want false", d)
		}
	}
}

func TestParseDomainsTrimsReportsAndDoesNotPanic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "records.conf")
	// Line 3 is the legacy form, an id after the name. The id is dropped rather than kept
	// as part of the name: that number is precisely what used to go stale and point an
	// update at the wrong record.
	content := "# comment\n\n  example.org = 12323 \nblog.example.org\nlocalhost\ndup.org\ndup.org=2\n= orphan\nW.ORG=1=2\n"
	if err := os.WriteFile(path, []byte(content), 0660); err != nil {
		t.Fatal(err)
	}

	names, problems := parseDomains(path)

	want := []string{"blog.example.org", "dup.org", "example.org", "w.org"}
	if !slices.Equal(names, want) {
		t.Errorf("names = %v, want %v (trimmed, lower-cased, sorted, legacy ids dropped)", names, want)
	}
	if len(problems) != 3 {
		t.Errorf("want 3 problems (localhost, duplicate, orphan), got %d: %v", len(problems), problems)
	}
	for _, p := range problems {
		if !strings.Contains(p, "records.conf:") {
			t.Errorf("problem should name file and line: %q", p)
		}
	}
	if !strings.Contains(strings.Join(problems, "|"), "records.conf:5") {
		t.Errorf("localhost is on line 5 and should be reported with that line number: %v", problems)
	}
}

// A missing config used to be log.Fatalf. A transiently unavailable bind mount should not
// take the daemon down; it is reported and the cycle is skipped.
func TestParseDomainsMissingFileIsReported(t *testing.T) {
	domains, problems := parseDomains(filepath.Join(t.TempDir(), "absent.conf"))
	if len(domains) != 0 {
		t.Errorf("expected no domains, got %v", domains)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "cannot open") {
		t.Errorf("expected one 'cannot open' problem, got %v", problems)
	}
}

func TestBriefKeepsTheLogOnOneLine(t *testing.T) {
	html := "<html><head>\n<title>503 Server Error</title>\n</head><body>Service error -27.</body></html>\n"
	got := brief(html, 200)
	if strings.ContainsAny(got, "\n\r") {
		t.Errorf("brief returned multiple lines: %q", got)
	}
	if !strings.Contains(got, "503 Server Error") {
		t.Errorf("the useful detail was lost: %q", got)
	}
	if brief("", 200) != "" {
		t.Error("empty body should stay empty")
	}
}

func TestBriefTruncatesOnRuneBoundaries(t *testing.T) {
	got := brief(strings.Repeat("a", 500), 200)
	if len(got) != 203 || !strings.HasSuffix(got, "...") {
		t.Errorf("truncation wrong: len=%d suffix=%v", len(got), strings.HasSuffix(got, "..."))
	}

	// Estonian text is everywhere in this project's neighbourhood; a byte slice could cut
	// a multi-byte character and emit invalid UTF-8.
	multi := strings.Repeat("õ", 500)
	got = brief(multi, 10)
	if got != strings.Repeat("õ", 10)+"..." {
		t.Errorf("not rune-safe: %q", got)
	}
	if !utf8.ValidString(got) {
		t.Error("brief produced invalid UTF-8")
	}
}

func TestWriteAtomicWritesWholeFileAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "records.conf")

	if err := writeAtomic(path, []byte("example.org=1\n"), 0660); err != nil {
		t.Fatalf("writeAtomic: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "example.org=1\n" {
		t.Fatalf("content = %q, err = %v", data, err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0660 {
		t.Errorf("perm = %v, want 0660", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("temporary files left behind: %v", entries)
	}
}

// The point of the temp-file-and-rename: if the write cannot happen, the previous file
// must still be there in one piece. os.WriteFile would already have truncated it.
func TestWriteAtomicLeavesOriginalIntactOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "records.conf")
	if err := os.WriteFile(path, []byte("stackoverlord.dev=845134\n"), 0660); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0500); err != nil { // read+execute, no creating files
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0700) // let t.TempDir clean up

	if err := writeAtomic(path, []byte("GARBAGE"), 0660); err == nil {
		t.Skip("writeAtomic unexpectedly succeeded - running with permissions that ignore the mode")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "stackoverlord.dev=845134\n" {
		t.Errorf("original was modified: %q", data)
	}
}

// Feeding the comment block back in must be a fixed point. Without collapsing blank runs
// the file gains a newline every cycle and grows forever.
func TestCommentLinesIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "records.conf")
	if err := os.WriteFile(path, []byte("# one\n\n# two\n\n\nexample.org=1\n\n"), 0660); err != nil {
		t.Fatal(err)
	}
	first := commentLines(path)
	for i := 0; i < 5; i++ {
		if err := writeAtomic(path, []byte(first), 0660); err != nil {
			t.Fatal(err)
		}
		if got := commentLines(path); got != first {
			t.Fatalf("comment block grew on round %d: %q -> %q", i+1, first, got)
		}
	}
}

func TestUpdateDomainListWritesNamesAndKeepsComments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "records.conf")
	if err := os.WriteFile(path, []byte("# my dyndns hosts\n# keep this note\n\n"), 0660); err != nil {
		t.Fatal(err)
	}
	names := []string{"b.org", "a.org"}

	if err := updateDomainList(path, names); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	want := "# my dyndns hosts\n# keep this note\n\na.org\nb.org\n"
	if string(data) != want {
		t.Errorf("got:\n%q\nwant:\n%q", data, want)
	}

	// Reading it back must give the same names: the rewrite is a fixed point, not something
	// that drifts a little further every cycle.
	reread, problems := parseDomains(path)
	if len(problems) != 0 || !slices.Equal(reread, []string{"a.org", "b.org"}) {
		t.Errorf("round trip through records.conf changed the list: %v %v", reread, problems)
	}

	// Same data again: the file must not be touched, so the mtime stays put.
	before, _ := os.Stat(path)
	time.Sleep(1100 * time.Millisecond)
	if err := updateDomainList(path, names); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("unchanged config was rewritten: %v -> %v", before.ModTime(), after.ModTime())
	}
}

// The defaults are what the container relies on; -conf is what lets the binary run
// anywhere else. Both are asserted, because a silent change to the default would break
// every existing deployment while the flag itself still looked fine.
func TestConfPathsFollowTheConfDir(t *testing.T) {
	t.Cleanup(func() { resolveConfPaths("conf") })

	resolveConfPaths("conf")
	if recordsConfPath != "conf/records.conf" {
		t.Errorf("default changed: %q", recordsConfPath)
	}

	dir := t.TempDir()
	resolveConfPaths(dir)
	if recordsConfPath != filepath.Join(dir, "records.conf") {
		t.Errorf("-conf not applied: %q", recordsConfPath)
	}

	if err := os.WriteFile(recordsConfPath, []byte("example.org=12323\n"), 0o660); err != nil {
		t.Fatal(err)
	}
	names, problems := parseDomains(recordsConfPath)
	if len(names) != 1 || len(problems) != 0 || names[0] != "example.org" {
		t.Errorf("reading from the -conf directory failed: %v %v", names, problems)
	}
}
