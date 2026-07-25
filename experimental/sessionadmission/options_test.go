package sessionadmission

import (
	"encoding/json"
	"testing"
	"time"
)

func mustParseOptions(t *testing.T, raw string) Options {
	t.Helper()
	var opts Options
	if err := json.Unmarshal([]byte(raw), &opts); err != nil {
		t.Fatalf("failed to parse options: %v", err)
	}
	return opts
}

func TestOptions_LimitSourcePerUserAndDefault(t *testing.T) {
	opts := mustParseOptions(t, `{
		"enabled": true,
		"default_limit": 2,
		"users": [
			{"uuid": "aaa", "max_devices": 5},
			{"uuid": "bbb", "max_devices": 0}
		]
	}`)
	ls := NewLimitSource(opts)

	if got := ls.Limit("aaa"); got != 5 {
		t.Fatalf("per-user limit: expected 5, got %d", got)
	}
	if got := ls.Limit("bbb"); got != 0 {
		t.Fatalf("explicit 0 (unlimited) user: expected 0, got %d", got)
	}
	// Unlisted user falls back to default_limit.
	if got := ls.Limit("ccc"); got != 2 {
		t.Fatalf("default fallback: expected 2, got %d", got)
	}
}

func TestOptions_DefaultLimitAbsent(t *testing.T) {
	opts := mustParseOptions(t, `{"enabled": true}`)
	ls := NewLimitSource(opts)
	// No default_limit and no users -> 0 (unlimited) for everyone.
	if got := ls.Limit("anyone"); got != 0 {
		t.Fatalf("absent default_limit should be 0, got %d", got)
	}
}

func TestOptions_FailureModeParsing(t *testing.T) {
	cases := []struct {
		raw  string
		want AdmissionFailureMode
	}{
		{`{"failure_mode": "reject"}`, FailClose},
		{`{"failure_mode": "allow"}`, FailOpen},
		{`{"failure_mode": ""}`, FailOpen},
		{`{}`, FailOpen},
		{`{"failure_mode": "bogus"}`, FailOpen},
	}
	for _, c := range cases {
		opts := mustParseOptions(t, c.raw)
		if got := opts.FailureModeValue(); got != c.want {
			t.Fatalf("raw %s: expected %q, got %q", c.raw, c.want, got)
		}
	}
}

func TestOptions_SlotGracePeriodParsing(t *testing.T) {
	// Absent -> disabled (0).
	if got := mustParseOptions(t, `{}`).GracePeriod(); got != 0 {
		t.Fatalf("absent grace period should be 0, got %v", got)
	}
	// Explicit "0s" -> disabled.
	if got := mustParseOptions(t, `{"slot_grace_period": "0s"}`).GracePeriod(); got != 0 {
		t.Fatalf("0s grace period should be 0, got %v", got)
	}
	// Non-zero parsed correctly.
	if got := mustParseOptions(t, `{"slot_grace_period": "30s"}`).GracePeriod(); got != 30*time.Second {
		t.Fatalf("expected 30s, got %v", got)
	}
	if got := mustParseOptions(t, `{"slot_grace_period": "5m"}`).GracePeriod(); got != 5*time.Minute {
		t.Fatalf("expected 5m, got %v", got)
	}
}

func TestParseFailureMode(t *testing.T) {
	if ParseFailureMode("reject") != FailClose {
		t.Fatal("reject should map to FailClose")
	}
	for _, s := range []string{"allow", "", "ALLOW", "reject "} {
		if ParseFailureMode(s) != FailOpen {
			t.Fatalf("%q should map to FailOpen", s)
		}
	}
}
