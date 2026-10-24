package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Configuration, read once by getEnv() before the loop starts. The values shown are the
// defaults; the environment overrides them.
//
// The two intervals are real time.Duration values. They used to hold a bare count inside
// a Duration - so 30 meant 30 nanoseconds - and were multiplied by time.Second or
// time.Minute at every use site, which worked only because the two mistakes cancelled
// out, and is why the log used to read "Trying again in 30ns seconds".
var (
	// Credentials for the Zone API. Deliberately ZONE_USERNAME and ZONE_API_KEY rather
	// than USERNAME and PASSWORD: pam puts USERNAME in every login shell, so a missing
	// setting was silently satisfied by the operator's login name and turned into a
	// permanent 401 instead of a startup error; and PASSWORD was never a password, it is
	// an API key.
	zoneUsername string
	zoneAPIKey   string

	NETWORK_RETRY_DELAY = 10 * time.Second // wait between retries; .env gives seconds
	CHECK_FREQUENCY     = 5 * time.Minute  // wait between cycles; .env gives minutes
	MAX_ATTEMPTS        = 5                // tries per operation per cycle
)

// maxUnusableCycles is how many cycles in a row may find nothing usable in the config
// before the process gives up. A single occurrence is worth riding out - a bind mount
// mid-update, an editor mid-save - but a run of them means a human has something to fix,
// and a daemon that quietly does nothing forever looks healthy from the outside.
const maxUnusableCycles = 3

// logLevel gates every log call. The handler holds a pointer to it, so getEnv can raise
// or lower the level after startup; its zero value is LevelInfo.
var logLevel slog.LevelVar

// Operator choices, filled in from the command line. They are package variables only so
// that the flag package can write them; plan() still receives them as an argument, which
// is where the decisions a test needs to cover live.
var (
	dryRun         bool
	allowEmptyZone bool
)

// setupLogging points the default logger at w (stdout when nil) with the current level.
// Taking the writer as an argument keeps the thing worth testing - "does LOG_LEVEL=info
// really hide the routine chatter?" - assertable instead of merely intended.
func setupLogging(w io.Writer) {
	if w == nil {
		w = os.Stdout
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: &logLevel,
	})))
}

func main() {
	// Logging is set up first, so that everything below - including a rejected
	// environment - reports through it. Output is stdout only: Docker captures it,
	// docker compose logs reads it back, and the container's logging driver is what caps
	// how much is kept (see docker-compose.yml). This process used to keep a second copy
	// in logs/updater.log, which grew to 6 MB without a rotation and reproduced every
	// line of the routine polling.
	setupLogging(nil)

	// The one thing that used to pin this binary to /zone-dyndns: the config path was a
	// compile-time constant relative to the working directory, so running it anywhere else
	// meant editing the source and rebuilding.
	flag.StringVar(&confDir, "conf", "conf", "directory holding records.conf")
	flag.BoolVar(&dryRun, "dry-run", false,
		"report what would change and send no create or update request")
	flag.BoolVar(&allowEmptyZone, "allow-empty-zone", false,
		"allow creating a record in a zone whose listing came back empty")
	flag.Parse()
	resolveConfPaths(confDir)

	if err := getEnv(); err != nil {
		slog.Error("the environment is not usable, nothing to do",
			"err", err, "hint", "see .env.example")
		os.Exit(1)
	}
	slog.Info("zone-dyndns starting",
		"conf_dir", confDir,
		"check_frequency", CHECK_FREQUENCY,
		"retry_delay", NETWORK_RETRY_DELAY,
		"max_attempts", MAX_ATTEMPTS,
		"log_level", logLevel.Level().String(),
		"dry_run", dryRun)

	// SIGTERM (docker stop) and SIGINT (Ctrl-C) cancel ctx. Nothing is killed mid-flight:
	// the cycle in progress finishes the request it is on, refuses to start the next one,
	// and main returns from the select below. See requestContext for why the in-flight
	// request is deliberately left alone.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	unusableCycles := 0
	ticker := time.NewTicker(CHECK_FREQUENCY)
	defer ticker.Stop()

	for {
		if runOnce(ctx) {
			unusableCycles = 0
		} else {
			unusableCycles++
			if unusableCycles >= maxUnusableCycles {
				// Exit non-zero so the restart policy and `docker ps` make this visible,
				// rather than logging politely where nobody looks.
				slog.Error("giving up, the config has produced nothing usable for several cycles",
					"cycles", unusableCycles, "path", recordsConfPath)
				os.Exit(1)
			}
			slog.Warn("nothing usable in the config, skipping this cycle",
				"cycles", unusableCycles, "max", maxUnusableCycles)
		}

		// The only wait in the program. It used to be three - one at the bottom of the loop
		// and one in each early-continue - and the copy in the public-IP path had to be
		// remembered by hand, which is the kind of thing that gets missed and turns into a
		// busy loop. Waiting here also means the signal is noticed instead of being slept
		// through.
		select {
		case <-ctx.Done():
			slog.Info("shutting down", "reason", ctx.Err())
			return
		case <-ticker.C:
		}
	}
}

// runOnce performs one reconcile cycle and reports whether the configuration yielded
// anything to act on.
//
// The cycle keeps no state. It asks Zone which A-records exist in each zone involved,
// compares that with records.conf, and sends only the requests that turn one into the
// other. There is no local copy of the published address and no stored record ids, so
// there is nothing that can drift out of date, nothing to rebuild after a wiped volume,
// and a failed cycle is simply retried against live data by the next one.
//
// It is a function rather than part of main so the loop has one wait instead of three, so
// that SIGTERM means "finish this cycle, then stop", and so that it is reachable from a
// test.
func runOnce(ctx context.Context) bool {
	names, problems := parseDomains(recordsConfPath)
	for _, problem := range problems {
		slog.Warn("config problem", "problem", problem)
	}
	if len(names) == 0 {
		return false
	}

	var publicIp string
	getPublicIp(ctx, &publicIp) // a pointer, so a retry can fill it in
	if publicIp == "" {
		// getPublicIp retried and gave up. With no address there is nothing to compare
		// against and nothing correct to publish, so the cycle is skipped rather than
		// guessed at.
		slog.Warn("public IP is unknown, skipping this cycle")
		return true
	}

	zones, warnings := zonesNeeded(names)
	for _, warning := range warnings {
		slog.Warn("skipping domain, cannot determine its zone", "problem", warning)
	}

	// One listing per zone. The whole plan rests on them, which is why a zone that could
	// not be read is skipped instead of assumed empty: "no records" and "no answer" must
	// not mean the same thing, or a transient failure becomes duplicate A-records.
	client := zoneClient{}
	listings := make(map[string][]Record, len(zones))
	for _, zone := range zones {
		if ctx.Err() != nil {
			slog.Info("shutting down, leaving the rest of this cycle", "zone", zone)
			break
		}
		records, err := client.listA(ctx, zone)
		if err != nil {
			slog.Warn("could not read this zone's records, leaving it alone this cycle",
				"zone", zone, "err", err)
			continue
		}
		slog.Debug("listed zone", "zone", zone, "records", len(records))
		listings[zone] = records
	}

	p := plan(names, listings, publicIp, PlanOptions{AllowEmptyZone: allowEmptyZone})
	for _, warning := range p.Warnings {
		slog.Warn("leaving something alone", "reason", warning)
	}
	if len(p.Unmanaged) > 0 {
		// Worth seeing at debug: the tool states what it is deliberately not touching,
		// rather than letting "it ignored my other records" be an assumption.
		slog.Debug("leaving unmanaged records alone", "count", len(p.Unmanaged),
			"records", p.Unmanaged)
	}

	// records.conf is the human's input, so it is only rewritten to canonicalise it -
	// sorted, one name per line, legacy "=id" suffix dropped. Nothing depends on what is
	// on disk, which is why a failure here is a warning rather than a skipped cycle.
	//
	// A dry run writes nothing at all, including this: "show me what you would do" should
	// not leave the operator's file changed underneath them.
	if dryRun {
		slog.Debug("dry run: records.conf left as it is", "path", recordsConfPath)
	} else if err := updateDomainList(recordsConfPath, names); err != nil {
		slog.Warn("could not tidy the domain list locally", "err", err)
	}

	if len(p.Actions) == 0 {
		slog.Debug("every record already points at this address",
			"ip", publicIp, "domains", p.Unchanged)
		return true
	}

	s := apply(ctx, client, p, publicIp, dryRun)
	if s.changed() {
		slog.Info("cycle finished", "created", s.Created, "updated", s.Updated,
			"failed", s.Failed, "unchanged", p.Unchanged, "dry_run", dryRun)
	}
	if s.Failed > 0 {
		slog.Error("some records could not be changed",
			"failed", s.Failed, "planned", len(p.Actions))
	}
	return true
}

// getEnv reads and validates every setting from the environment. It reports all problems
// at once and names the variable: previously a missing setting produced
// `strconv.Atoi: parsing "": invalid syntax`, which told an operator nothing about which
// setting was broken, and only the first problem was ever reported.
func getEnv() error {
	var problems []error

	// Quotes and surrounding whitespace are trimmed because docker-compose passes
	// through whatever the .env file contains, and quoted values are common.
	zoneUsername = strings.Trim(os.Getenv("ZONE_USERNAME"), " \"'\r\n\t")
	zoneAPIKey = strings.Trim(os.Getenv("ZONE_API_KEY"), " \"'\r\n\t")
	if zoneUsername == "" {
		problems = append(problems, errors.New("ZONE_USERNAME is not set"))
	}
	if zoneAPIKey == "" {
		problems = append(problems, errors.New("ZONE_API_KEY is not set"))
	}

	if d, err := envDuration("NETWORK_RETRY_DELAY", time.Second, "seconds"); err != nil {
		problems = append(problems, err)
	} else {
		NETWORK_RETRY_DELAY = d
	}
	if d, err := envDuration("CHECK_FREQUENCY", time.Minute, "minutes"); err != nil {
		problems = append(problems, err)
	} else {
		CHECK_FREQUENCY = d
	}
	if n, err := envInt("MAX_ATTEMPTS", MAX_ATTEMPTS); err != nil {
		problems = append(problems, err)
	} else {
		MAX_ATTEMPTS = n
	}
	if level, err := envLogLevel("LOG_LEVEL"); err != nil {
		problems = append(problems, err)
	} else {
		logLevel.Set(level)
	}

	return errors.Join(problems...)
}

// envLogLevel reads LOG_LEVEL=debug|info|warn|error, case-insensitive, defaulting to info.
// An unrecognised value is an error naming the variable, like every other setting: a
// silent fallback would leave someone debugging at the level they thought they had set.
func envLogLevel(name string) (slog.Level, error) {
	raw := strings.ToLower(strings.Trim(os.Getenv(name), " \"'\r\n\t"))
	switch raw {
	case "":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return slog.LevelInfo, fmt.Errorf("%s=%q is not one of debug, info, warn or error", name, raw)
}

// envDuration reads a whole number of an explicit unit: NETWORK_RETRY_DELAY=30 with
// time.Second becomes 30s, CHECK_FREQUENCY=10 with time.Minute becomes 10m.
//
// The unit is a parameter rather than "everything is seconds" on purpose. CHECK_FREQUENCY
// has always been minutes, so an existing .env with CHECK_FREQUENCY=10 keeps meaning ten
// minutes; silently reinterpreting it as seconds would have made the updater poll sixty
// times faster against a live DNS zone.
//
// A missing, unparseable or non-positive value is an error naming the variable.
// Non-positive matters: CHECK_FREQUENCY=0 spins the loop and NETWORK_RETRY_DELAY=0 turns
// every retry back into a busy loop.
func envDuration(name string, unit time.Duration, unitName string) (time.Duration, error) {
	raw := strings.Trim(os.Getenv(name), " \"'\r\n\t")
	if raw == "" {
		return 0, fmt.Errorf("%s is not set (expected a whole number of %s)", name, unitName)
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a whole number of %s", name, raw, unitName)
	}
	if n <= 0 {
		return 0, fmt.Errorf("%s=%d must be greater than zero", name, n)
	}
	return time.Duration(n) * unit, nil
}

// envInt reads an optional whole number, falling back when it is unset.
func envInt(name string, fallback int) (int, error) {
	raw := strings.Trim(os.Getenv(name), " \"'\r\n\t")
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a whole number", name, raw)
	}
	if n < 1 {
		return 0, fmt.Errorf("%s=%d must be at least 1", name, n)
	}
	return n, nil
}
