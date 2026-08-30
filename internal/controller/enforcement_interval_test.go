/*
Copyright (c) 2025 Stan Kozarev.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"testing"
	"time"

	saltv1alpha1 "github.com/stanimir-kozarev/vcf-salt-operator/api/v1alpha1"
)

// cfgWithInterval builds the minimum SaltKeyConfig enforcementDue reads.
func cfgWithInterval(interval string) *saltv1alpha1.SaltKeyConfig {
	return &saltv1alpha1.SaltKeyConfig{
		Spec: saltv1alpha1.SaltKeyConfigSpec{EnforcementInterval: interval},
	}
}

const testUID = "8f14e45f-ea8f-4b9a-9c1e-2f5b7d3a6c04"

// rfc3339Ago renders a timestamp d in the past, in the format the operator writes to
// AnnotationHighstateTime.
func rfc3339Ago(now time.Time, d time.Duration) string {
	return now.Add(-d).UTC().Format(time.RFC3339)
}

// TestEnforcementDue_DisabledWhenUnset is the case that matters most for an existing
// deployment: a SaltKeyConfig that predates this field must behave exactly as it did
// before, dormant and never re-run on a timer. A regression here would silently start
// dispatching highstates across every enrolled namespace, which is why this is the first
// test rather than an afterthought to the happy path.
func TestEnforcementDue_DisabledWhenUnset(t *testing.T) {
	now := time.Now().UTC()
	// A last highstate far older than any plausible interval: if the feature were
	// accidentally enabled by default, this input would certainly trigger it.
	last := rfc3339Ago(now, 365*24*time.Hour)

	for _, tc := range []struct {
		name string
		cfg  *saltv1alpha1.SaltKeyConfig
	}{
		{"field absent", cfgWithInterval("")},
		{"nil config", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			due, wait := enforcementDue(tc.cfg, testUID, last, now)
			if due {
				t.Fatal("enforcement fired with no interval configured, which would change behaviour for every existing SaltKeyConfig")
			}
			if wait != 0 {
				t.Fatalf("wait = %v, want 0 so the VM stays dormant rather than being requeued forever", wait)
			}
		})
	}
}

// TestEnforcementDue_NotYetDue covers the ordinary in-between case: configured, but the
// interval has not elapsed. The VM must not run, and must be requeued for roughly the
// remaining time rather than dropped (which would leave it dormant until an unrelated
// event woke it).
func TestEnforcementDue_NotYetDue(t *testing.T) {
	now := time.Now().UTC()
	last := rfc3339Ago(now, time.Hour)

	due, wait := enforcementDue(cfgWithInterval("24h"), testUID, last, now)
	if due {
		t.Fatal("enforcement fired one hour into a 24h interval")
	}
	if wait <= 0 {
		t.Fatalf("wait = %v, want a positive requeue so the VM is re-examined when it comes due", wait)
	}
	// 23h remaining, plus this VM's jitter, which is bounded by interval/10.
	if wait < 23*time.Hour || wait > 23*time.Hour+(24*time.Hour/10) {
		t.Fatalf("wait = %v, want ~23h plus jitter under 2h24m", wait)
	}
}

// TestEnforcementDue_FiresOnceElapsed is the positive case.
func TestEnforcementDue_FiresOnceElapsed(t *testing.T) {
	now := time.Now().UTC()
	// Well past the interval and its maximum jitter, so this is unambiguously due.
	last := rfc3339Ago(now, 48*time.Hour)

	due, wait := enforcementDue(cfgWithInterval("24h"), testUID, last, now)
	if !due {
		t.Fatal("enforcement did not fire 48h into a 24h interval")
	}
	if wait != 0 {
		t.Fatalf("wait = %v, want 0 when the caller is about to dispatch", wait)
	}
}

// TestEnforcementDue_NoCompletedHighstate guards the fail-quiet direction. A VM with no
// recorded highstate time has nothing to measure from, and the safe answer is to do
// nothing rather than to treat "no timestamp" as "infinitely overdue" and dispatch.
func TestEnforcementDue_NoCompletedHighstate(t *testing.T) {
	now := time.Now().UTC()

	for _, tc := range []struct{ name, last string }{
		{"empty", ""},
		{"unparseable", "not-a-timestamp"},
		{"wrong format", "2026-08-30 12:00:00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			due, wait := enforcementDue(cfgWithInterval("24h"), testUID, tc.last, now)
			if due {
				t.Fatalf("enforcement fired on an unusable highstate-time %q, which would dispatch against every such VM at once", tc.last)
			}
			if wait != 0 {
				t.Fatalf("wait = %v, want 0", wait)
			}
		})
	}
}

// TestEnforcementDue_RejectsUnusableInterval covers values that parse as durations but
// cannot schedule anything. Disabling is the safe reading: a zero or negative interval
// read as "always due" would put every Ready VM into a dispatch loop.
func TestEnforcementDue_RejectsUnusableInterval(t *testing.T) {
	now := time.Now().UTC()
	last := rfc3339Ago(now, 365*24*time.Hour)

	for _, interval := range []string{"0s", "-1h", "garbage"} {
		t.Run(interval, func(t *testing.T) {
			due, wait := enforcementDue(cfgWithInterval(interval), testUID, last, now)
			if due {
				t.Fatalf("enforcement fired on interval %q", interval)
			}
			if wait != 0 {
				t.Fatalf("wait = %v, want 0", wait)
			}
		})
	}
}

// TestEnforcementJitter_StableAndBounded asserts the two properties the jitter exists for.
// Stability matters because an unstable offset would move a VM's due time on every
// reconcile, so it could either never come due or come due repeatedly.
func TestEnforcementJitter_StableAndBounded(t *testing.T) {
	const interval = 24 * time.Hour
	maxJitter := interval / 10

	first := enforcementJitter(testUID, interval)
	for range 100 {
		if got := enforcementJitter(testUID, interval); got != first {
			t.Fatalf("jitter is not stable for a fixed UID: %v then %v", first, got)
		}
	}
	if first < 0 || first >= maxJitter {
		t.Fatalf("jitter %v outside [0, %v)", first, maxJitter)
	}
}

// TestEnforcementJitter_SpreadsAcrossVMs is the property that replaces a pacing subsystem:
// VMs onboarded in one batch share a bootstrap time, so without distinct jitter they would
// stay in lockstep and come due together forever.
func TestEnforcementJitter_SpreadsAcrossVMs(t *testing.T) {
	const interval = 24 * time.Hour

	seen := map[time.Duration]bool{}
	for _, uid := range []string{
		"3f2504e0-4f89-11d3-9a0c-0305e82c3301",
		"3f2504e0-4f89-11d3-9a0c-0305e82c3302",
		"3f2504e0-4f89-11d3-9a0c-0305e82c3303",
		"3f2504e0-4f89-11d3-9a0c-0305e82c3304",
		"3f2504e0-4f89-11d3-9a0c-0305e82c3305",
	} {
		seen[enforcementJitter(uid, interval)] = true
	}
	if len(seen) < 4 {
		t.Fatalf("5 near-identical UIDs produced only %d distinct offsets, so a batch would stay near lockstep", len(seen))
	}
}

// TestEnforcementJitter_ZeroWhenUnusable keeps the helper total: a missing UID or an
// interval too small to divide must degrade to no jitter rather than panic on a modulo by
// zero.
func TestEnforcementJitter_ZeroWhenUnusable(t *testing.T) {
	if got := enforcementJitter("", 24*time.Hour); got != 0 {
		t.Fatalf("jitter with no UID = %v, want 0", got)
	}
	if got := enforcementJitter(testUID, 5*time.Nanosecond); got != 0 {
		t.Fatalf("jitter with an interval below its own divisor = %v, want 0", got)
	}
}
