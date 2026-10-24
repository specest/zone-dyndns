package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// httpClient is shared by every outbound call: the IP service and all Zone API requests.
// A zero Timeout means no deadline at all - a black-holed TCP connect, or a server that
// accepts the connection and never replies, parks the goroutine permanently with nothing
// in the log, and the process still looks healthy to docker's restart policy. 20s sits
// well above the sub-second normal round trip and above the ~10s a failed DNS lookup
// already costs (see updater.log, 2026-10-04), while still bounding a hang.
var httpClient = &http.Client{Timeout: 20 * time.Second}

// The two endpoints. Variables rather than literals so that a test can point the updater
// at a stand-in server: without this the cycle cannot be exercised in-repo at all, and
// every end-to-end check had to be done in a throwaway copy of the source with sed.
// Nothing but tests reassigns them.
var (
	zoneAPIBase  = "https://api.zone.eu/v2"
	ipServiceURL = "https://ifconfig.me/ip"
)

// requestContext returns the context for one outbound call.
//
// It is deliberately context.WithoutCancel: shutting down must not abort a request that
// is already on the wire, because an interrupted PUT is exactly how Zone ends up pointing
// at the new address while ip.conf still holds the old one - a discrepancy that survives
// the restart and is invisible afterwards. Bounding a hung request is httpClient.Timeout's
// job. What shutdown does stop is starting *new* work: retryWait refuses to retry or to
// sleep once the lifecycle context is done.
func requestContext(ctx context.Context) context.Context {
	return context.WithoutCancel(ctx)
}

// maxResponseBytes caps how much of a response body is read into memory. Zone's real
// answers are a few hundred bytes, so 1 MiB is orders of magnitude of headroom; without a
// limit, a misbehaving or compromised endpoint decides how much memory this process
// allocates.
const maxResponseBytes = 1 << 20

// retryWait decides whether an operation is worth trying again, pausing first when it
// is. Every retry path goes through here so that all of them wait - two of them used to
// recurse with no delay at all, which turns a persistent failure into a busy loop - and
// so that all of them stop. MAX_ATTEMPTS caps a single cycle; a new cycle starts in
// CHECK_FREQUENCY anyway, so a transient failure gets retried regardless, and retrying
// forever inside one cycle would only give an ever-growing stack and a rate-limit
// conversation with Zone.
func retryWait(ctx context.Context, what string, attempt int) bool {
	if ctx.Err() != nil {
		slog.Info("shutting down, not retrying", "op", what, "attempt", attempt)
		return false
	}
	if attempt >= MAX_ATTEMPTS {
		slog.Error("giving up for this cycle", "op", what, "attempts", attempt,
			"next_try", "next cycle")
		return false
	}
	slog.Warn("operation failed, retrying", "op", what, "attempt", attempt,
		"max_attempts", MAX_ATTEMPTS, "wait", NETWORK_RETRY_DELAY)
	// An interruptible wait: `docker stop` used to sit out the remaining seconds of a
	// retry delay, and a delay longer than Docker's 10s grace period ends in SIGKILL,
	// which is the ungraceful death this change exists to avoid.
	timer := time.NewTimer(NETWORK_RETRY_DELAY)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		slog.Info("shutting down, abandoning the retry", "op", what, "attempt", attempt)
		return false
	case <-timer.C:
		return true
	}
}

// brief renders a response body as a single capped log line. Bodies used to be logged
// verbatim, so one error page from the IP service arrived as nine separate lines and
// broke grep on updater.log (see 2026-08-18). Cut on runes, so a multi-byte body cannot
// be split mid-character.
func brief(body string, max int) string {
	collapsed := strings.Join(strings.Fields(body), " ")
	r := []rune(collapsed)
	if len(r) > max {
		return string(r[:max]) + "..."
	}
	return collapsed
}

func getPublicIp(ctx context.Context, publicIp *string) {
	getPublicIpAttempt(ctx, publicIp, 1)
}

// getPublicIpAttempt stores the current public IPv4 address in *publicIp. If it cannot,
// *publicIp is left empty so that main can skip the cycle instead of acting on an
// address it never actually learned.
func getPublicIpAttempt(ctx context.Context, publicIp *string, attempt int) {
	url := ipServiceURL
	// Named from the variable rather than hardcoded: this logged "ipify" while calling
	// ifconfig.me, which sent more than one reader to the wrong status page.
	slog.Debug("querying the IP service", "url", url)
	req, err := http.NewRequestWithContext(requestContext(ctx), http.MethodGet, url, nil)
	if err != nil {
		slog.Error("cannot build the IP service request", "url", url, "err", err)
		return
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		slog.Warn("no response from the IP service", "url", url, "err", err)
		if retryWait(ctx, "getting the public IP", attempt) {
			getPublicIpAttempt(ctx, publicIp, attempt+1)
		}
		return
	}
	defer resp.Body.Close()

	// A non-200 response still carries a body, usually an error page. Treating that
	// body as an address once made this updater try to repoint every A-record at a
	// 503 page from the IP service (see the 2026-08-18 incident in updater.log).
	if resp.StatusCode != http.StatusOK {
		slog.Warn("IP service returned an unexpected status, body ignored", "url", url,
			"status", resp.Status)
		if retryWait(ctx, "getting the public IP", attempt) {
			getPublicIpAttempt(ctx, publicIp, attempt+1)
		}
		return
	}

	ip, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		slog.Warn("failed to read the IP service response", "url", url, "err", err)
		if retryWait(ctx, "getting the public IP", attempt) {
			getPublicIpAttempt(ctx, publicIp, attempt+1)
		}
		return
	}

	// This value becomes the destination of live DNS records, so it has to be a
	// routable IPv4 address. Anything else means: leave DNS alone this cycle.
	newIp := strings.TrimSpace(string(ip))
	if !isRoutableIPv4(newIp) {
		// brief() collapses and truncates: a rejected body can be a whole HTML error
		// page, and it must stay on one line.
		slog.Warn("IP service response is not a routable IPv4 address, ignoring it",
			"url", url, "body", brief(string(ip), 200))
		if retryWait(ctx, "getting the public IP", attempt) {
			getPublicIpAttempt(ctx, publicIp, attempt+1)
		}
		return
	}

	slog.Debug("public IP resolved", "ip", newIp)
	*publicIp = newIp
}

// isRoutableIPv4 reports whether s is the textual form of an IPv4 address that is safe
// to publish as a DNS A-record destination. RFC1918 and CGNAT addresses pass, because
// pointing a record at a VPN- or LAN-side address is a legitimate use. Loopback,
// unspecified, link-local, multicast and broadcast addresses do not.
func isRoutableIPv4(s string) bool {
	ip := net.ParseIP(s)
	return ip != nil && ip.To4() != nil && ip.IsGlobalUnicast()
}

// parseDomains reads the list of (sub)domains to manage and returns them sorted, along
// with every line it had to reject. Reporting beats crashing: a single mistyped line used
// to reach getDomainRoot and panic the process.
//
// A line is a domain name. A value after "=" is still accepted and ignored, because
// records.conf written by older versions looked like "example.org=12323": that number was
// Zone's record id. It is no longer stored anywhere - when an id is needed it comes from
// the live listing - so a stale, hand-edited or copied-over file can no longer point an
// update at the wrong record, and the file cannot get out of sync with DNS.
func parseDomains(path string) ([]string, []string) {
	var names, problems []string
	seen := make(map[string]bool)

	f, err := os.Open(path)
	if err != nil {
		return names, []string{fmt.Sprintf("cannot open %s: %v", path, err)}
	}
	defer f.Close() // this descriptor used to be left to the garbage collector

	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		// Cut, not Split: a value containing '=' stays whole rather than being chopped at
		// the first one. Both sides are trimmed - "example.org = 12323" used to yield a
		// domain with a trailing space, which then went into the API URL.
		name, _, _ := strings.Cut(line, "=")
		name = strings.ToLower(strings.TrimSpace(name))

		if name == "" {
			problems = append(problems, fmt.Sprintf("%s:%d: no domain on this line", path, lineNo))
			continue
		}
		if seen[name] {
			problems = append(problems, fmt.Sprintf("%s:%d: %s is listed twice, keeping one entry", path, lineNo, name))
			continue
		}
		if !validDomain(name) {
			problems = append(problems, fmt.Sprintf("%s:%d: %q is not a valid (sub)domain", path, lineNo, name))
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	if err := scanner.Err(); err != nil {
		problems = append(problems, fmt.Sprintf("reading %s: %v", path, err))
	}
	sort.Strings(names)
	return names, problems
}

// validDomain accepts the hostnames this tool can act on: two or more labels of letters,
// digits and interior hyphens, none longer than 63 characters. Rejecting here means the
// message can name the file and line number, instead of a malformed domain surfacing
// later as a failed request or a panic.
func validDomain(domain string) bool {
	if domain == "" || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return false
	}
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			alnum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
			hyphen := c == '-' && i > 0 && i < len(label)-1
			if !alnum && !hyphen {
				return false
			}
		}
	}
	return true
}

// getDomainRoot returns the zone a (sub)domain belongs to, which is what the Zone API
// addresses: the last two labels.
//
// It used to index s[len(s)-2] unconditionally, which panicked with
// "index out of range [-1]" on a single-label name - one typo in records.conf was enough
// to kill the daemon, and restart:always turned that into a crash loop.
//
// Known limitation, unchanged by this: the last-two-labels rule is wrong for multi-part
// public suffixes (blog.example.co.uk resolves to co.uk). Fixing that needs a public
// suffix list; the tool has always behaved this way and now says so here rather than
// silently sending requests to the wrong zone.
func getDomainRoot(domain string) (string, error) {
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return "", fmt.Errorf("%q is not a (sub)domain: expected at least one dot", domain)
	}
	return labels[len(labels)-2] + "." + labels[len(labels)-1], nil
}

// Where the two config files live. The defaults match the container layout - the working
// directory holds the conf/ mount - and -conf overrides them once at startup, so the
// binary is no longer welded to /zone-dyndns and a test can point it at a temporary
// directory.
var (
	recordsConfPath = "conf/records.conf"
	confDir         = "conf"
)

// resolveConfPaths derives the config file location from the directory the -conf flag
// names.
func resolveConfPaths(dir string) {
	confDir = dir
	recordsConfPath = filepath.Join(dir, "records.conf")
}

// writeAtomic replaces path with data through a temporary file in the same directory and
// a rename. os.WriteFile truncates the target before it writes a single byte, so a
// docker stop, an OOM kill or a full disk landing in that window leaves records.conf
// empty - and the resource ids in it are the one piece of state that is painful to
// rebuild. rename(2) inside one filesystem is atomic, so a reader sees either the whole
// old file or the whole new one, never half of either.
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp")
	if err != nil {
		return err
	}
	// Removes the temporary file if anything below fails; a no-op once the rename moved
	// it out of the way.
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// Flush before the rename so the data is on disk before the directory entry changes.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), perm); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// commentLines returns the comment and blank lines already present in path, in their
// original order, so notes a user wrote into the config survive the updater rewriting it.
// A comment sitting between two entries moves up into this block: it is commentary, not
// state the updater has to track.
//
// Blank runs are collapsed to a single line and leading blanks dropped, which keeps this
// function idempotent. Without that, feeding its own output back in would add a newline
// every cycle and grow the file forever.
func commentLines(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var out []string
	prevBlank := true
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			if !prevBlank && len(out) > 0 {
				out = append(out, "")
				prevBlank = true
			}
		case strings.HasPrefix(trimmed, "#"):
			out = append(out, trimmed)
			prevBlank = false
		}
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, "\n") + "\n"
}

// updateDomainList keeps records.conf on disk in the canonical form: the comment block
// the user wrote, then the managed names, one per line, sorted. Map iteration order is
// randomised, so without the sort the file would rewrite itself every cycle; with the
// no-change-no-write check it is normally untouched, which also keeps its mtime stable.
func updateDomainList(path string, names []string) error {
	comments := commentLines(path)
	if comments == "" {
		comments = recordsComment
	}
	// Sorted here as well as in parseDomains: parseDomains is the only caller today, but a
	// file that reorders itself depending on how the caller built its slice is exactly the
	// churn this function exists to avoid.
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)

	var b strings.Builder
	b.WriteString(comments)
	for _, name := range sorted {
		b.WriteString(name + "\n")
	}
	text := b.String()

	if current, err := os.ReadFile(path); err == nil && string(current) == text {
		return nil // steady state: no write, no mtime change, no churn
	}
	if err := writeAtomic(path, []byte(text), 0660); err != nil {
		return fmt.Errorf("updating the domain list: %w", err)
	}
	return nil
}

var recordsComment string = `# (Sub)domains that should point at this machine's dynamic IP address.
# One name per line; blank lines and lines starting with # are kept as comments.
#
# example.org
# blog.example.org
#
# Older versions of this tool stored a Zone record id after each name
# ("example.org=12323"). Those lines still work - the number is ignored. The id is read
# from Zone when it is needed, so this file can never send an update to the wrong record.
#
# Records here are only created or repointed, never deleted. An A-record in the same zone
# that is not listed here is left alone and mentioned in the log at debug level.` + "\n"
