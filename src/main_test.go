package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// setEnv sets all five settings for a case, clearing the ones it does not provide.
// getEnv treats an empty value as unset, which is what these tests need.
func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for _, name := range []string{"ZONE_USERNAME", "ZONE_API_KEY", "NETWORK_RETRY_DELAY", "CHECK_FREQUENCY", "MAX_ATTEMPTS", "LOG_LEVEL"} {
		t.Setenv(name, kv[name])
	}
}

func TestGetEnv(t *testing.T) {
	valid := map[string]string{
		"ZONE_USERNAME":       "specest",
		"ZONE_API_KEY":        "apikey",
		"NETWORK_RETRY_DELAY": "30",
		"CHECK_FREQUENCY":     "10",
		"MAX_ATTEMPTS":        "5",
	}

	t.Run("valid values", func(t *testing.T) {
		setEnv(t, valid)
		if err := getEnv(); err != nil {
			t.Fatalf("getEnv() = %v, want no error", err)
		}
		if zoneUsername != "specest" || zoneAPIKey != "apikey" {
			t.Errorf("credentials = %q/%q", zoneUsername, zoneAPIKey)
		}
		if NETWORK_RETRY_DELAY != 30*time.Second {
			t.Errorf("NETWORK_RETRY_DELAY = %v, want 30s", NETWORK_RETRY_DELAY)
		}
		if CHECK_FREQUENCY != 10*time.Minute {
			t.Errorf("CHECK_FREQUENCY = %v, want 10m", CHECK_FREQUENCY)
		}
		if MAX_ATTEMPTS != 5 {
			t.Errorf("MAX_ATTEMPTS = %d, want 5", MAX_ATTEMPTS)
		}
	})

	// CHECK_FREQUENCY has always been minutes. If it ever becomes seconds, an existing
	// .env with CHECK_FREQUENCY=10 turns into a poll every ten seconds against a live
	// DNS zone. This case is the guard.
	t.Run("CHECK_FREQUENCY is minutes, NETWORK_RETRY_DELAY is seconds", func(t *testing.T) {
		setEnv(t, valid)
		if err := getEnv(); err != nil {
			t.Fatalf("getEnv() = %v", err)
		}
		if CHECK_FREQUENCY != 600*time.Second {
			t.Errorf("CHECK_FREQUENCY = %v; it must stay 600s (10 minutes), not 10 seconds", CHECK_FREQUENCY)
		}
		if NETWORK_RETRY_DELAY == 30*time.Minute {
			t.Error("NETWORK_RETRY_DELAY must be seconds, not minutes")
		}
	})

	// The trailing space sitting in .env.example broke strconv.Atoi("10 ").
	t.Run("trailing whitespace still parses", func(t *testing.T) {
		setEnv(t, map[string]string{
			"ZONE_USERNAME": "specest", "ZONE_API_KEY": "apikey",
			"NETWORK_RETRY_DELAY": "10 ", "CHECK_FREQUENCY": " 5",
		})
		if err := getEnv(); err != nil {
			t.Fatalf("getEnv() = %v, want surrounding spaces ignored", err)
		}
		if NETWORK_RETRY_DELAY != 10*time.Second || CHECK_FREQUENCY != 5*time.Minute {
			t.Errorf("got %v / %v", NETWORK_RETRY_DELAY, CHECK_FREQUENCY)
		}
	})

	t.Run("quoted values still parse", func(t *testing.T) {
		setEnv(t, map[string]string{
			"ZONE_USERNAME": `"specest"`, "ZONE_API_KEY": "'apikey'",
			"NETWORK_RETRY_DELAY": `"30"`, "CHECK_FREQUENCY": `'10'`,
		})
		if err := getEnv(); err != nil {
			t.Fatalf("getEnv() = %v, want quotes stripped", err)
		}
		if zoneUsername != "specest" || zoneAPIKey != "apikey" {
			t.Errorf("credentials not unquoted: %q/%q", zoneUsername, zoneAPIKey)
		}
	})

	t.Run("every problem is reported at once and names the variable", func(t *testing.T) {
		// ZONE_API_KEY unset, NETWORK_RETRY_DELAY unset, MAX_ATTEMPTS not a number.
		// MAX_ATTEMPTS has to be given a bad value to be reported: empty is valid for it.
		setEnv(t, map[string]string{
			"ZONE_USERNAME": "specest", "CHECK_FREQUENCY": "5", "MAX_ATTEMPTS": "many",
		})
		err := getEnv()
		if err == nil {
			t.Fatal("getEnv() = nil, want an error")
		}
		msg := err.Error()
		for _, want := range []string{"ZONE_API_KEY", "NETWORK_RETRY_DELAY", "MAX_ATTEMPTS"} {
			if !strings.Contains(msg, want) {
				t.Errorf("error %q should name %s", msg, want)
			}
		}
		if strings.Contains(msg, "invalid syntax") {
			t.Errorf("error %q still leaks a bare strconv message", msg)
		}
	})

	// CHECK_FREQUENCY=0 spins the loop; NETWORK_RETRY_DELAY=0 makes every retry a busy
	// loop. Both must be refused at startup rather than discovered in production.
	t.Run("zero and negative are refused", func(t *testing.T) {
		for _, bad := range []string{"0", "-5"} {
			setEnv(t, map[string]string{
				"ZONE_USERNAME": "u", "ZONE_API_KEY": "p",
				"NETWORK_RETRY_DELAY": bad, "CHECK_FREQUENCY": bad,
			})
			err := getEnv()
			if err == nil {
				t.Fatalf("getEnv() with %q = nil, want an error", bad)
			}
			if !strings.Contains(err.Error(), "NETWORK_RETRY_DELAY") || !strings.Contains(err.Error(), "CHECK_FREQUENCY") {
				t.Errorf("error %q should name both offending variables", err)
			}
		}
	})

	t.Run("non-numeric names the variable and the bad value", func(t *testing.T) {
		setEnv(t, map[string]string{
			"ZONE_USERNAME": "u", "ZONE_API_KEY": "p",
			"NETWORK_RETRY_DELAY": "ten", "CHECK_FREQUENCY": "five",
		})
		err := getEnv()
		if err == nil {
			t.Fatal("getEnv() = nil, want an error")
		}
		if !strings.Contains(err.Error(), `NETWORK_RETRY_DELAY="ten"`) {
			t.Errorf("error %q should quote the offending value", err)
		}
	})

	// LOG_LEVEL is the knob that turns the routine chatter off, so its spelling variants
	// are worth pinning down.
	t.Run("LOG_LEVEL", func(t *testing.T) {
		base := map[string]string{
			"ZONE_USERNAME": "u", "ZONE_API_KEY": "p",
			"NETWORK_RETRY_DELAY": "30", "CHECK_FREQUENCY": "10",
		}
		defer logLevel.Set(slog.LevelInfo)

		for _, c := range []struct {
			in   string
			want string // expected LevelVar rendering; "" means the value must be refused
		}{
			{"", "INFO"},
			{"debug", "DEBUG"},
			{"INFO", "INFO"},
			{" warn ", "WARN"},
			{"warning", "WARN"},
			{"error", "ERROR"},
			{"verbose", ""},
			{"1", ""},
		} {
			env := map[string]string{}
			for k, v := range base {
				env[k] = v
			}
			env["LOG_LEVEL"] = c.in
			setEnv(t, env)

			err := getEnv()
			if c.want == "" {
				if err == nil || !strings.Contains(err.Error(), "LOG_LEVEL") {
					t.Errorf("LOG_LEVEL=%q: got error %v, want one naming LOG_LEVEL", c.in, err)
				}
				continue
			}
			if err != nil {
				t.Errorf("LOG_LEVEL=%q: unexpected error %v", c.in, err)
				continue
			}
			if got := logLevel.Level().String(); got != c.want {
				t.Errorf("LOG_LEVEL=%q: level is %s, want %s", c.in, got, c.want)
			}
		}
	})

	t.Run("MAX_ATTEMPTS is optional", func(t *testing.T) {
		setEnv(t, map[string]string{
			"ZONE_USERNAME": "u", "ZONE_API_KEY": "p",
			"NETWORK_RETRY_DELAY": "30", "CHECK_FREQUENCY": "10",
		})
		MAX_ATTEMPTS = 5
		if err := getEnv(); err != nil {
			t.Fatalf("getEnv() = %v, want no error when MAX_ATTEMPTS is unset", err)
		}
		if MAX_ATTEMPTS != 5 {
			t.Errorf("MAX_ATTEMPTS = %d, want the default 5", MAX_ATTEMPTS)
		}

		setEnv(t, map[string]string{
			"ZONE_USERNAME": "u", "ZONE_API_KEY": "p",
			"NETWORK_RETRY_DELAY": "30", "CHECK_FREQUENCY": "10", "MAX_ATTEMPTS": "0",
		})
		if err := getEnv(); err == nil {
			t.Error("MAX_ATTEMPTS=0 should be refused: it would retry zero times")
		}
	})
}

// TestLogLevelGate is about what a level does, not how it parses. The claim being made -
// an unremarkable cycle is silent at info, and everything is visible at debug - only holds
// if the routine lines are logged at Debug. That cannot be seen from any single call site,
// so it is asserted here.
func TestLogLevelGateControlsTheRoutineChatter(t *testing.T) {
	t.Cleanup(func() {
		logLevel.Set(slog.LevelInfo)
		setupLogging(nil)
	})

	for _, c := range []struct {
		env                           string
		wantDebug, wantInfo, wantWarn bool
	}{
		{"", false, true, true},
		{"debug", true, true, true},
		{"warn", false, false, true},
		{"error", false, false, false},
	} {
		var buf bytes.Buffer
		setEnv(t, map[string]string{
			"ZONE_USERNAME": "u", "ZONE_API_KEY": "p",
			"NETWORK_RETRY_DELAY": "30", "CHECK_FREQUENCY": "10",
			"LOG_LEVEL": c.env,
		})
		if err := getEnv(); err != nil {
			t.Fatalf("getEnv() = %v", err)
		}
		setupLogging(&buf)

		slog.Debug("public IP unchanged", "ip", "203.0.113.7")
		slog.Info("record updated", "domain", "example.org", "status", "200 OK")
		slog.Warn("record update failed", "domain", "example.org", "status", "401 Unauthorized")

		out := buf.String()
		for _, want := range []struct {
			msg     string
			present bool
		}{
			{"public IP unchanged", c.wantDebug},
			{"record updated", c.wantInfo},
			{"record update failed", c.wantWarn},
		} {
			if got := strings.Contains(out, want.msg); got != want.present {
				t.Errorf("LOG_LEVEL=%q: %q present=%v, want %v (output %q)",
					c.env, want.msg, got, want.present, out)
			}
		}
		// The fields are the reason to use slog; they have to survive into the output.
		if c.wantWarn && !strings.Contains(out, "domain=example.org") {
			t.Errorf("expected structured fields, got %q", out)
		}
	}
}
