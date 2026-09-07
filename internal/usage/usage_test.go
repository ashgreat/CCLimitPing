package usage

import (
	"testing"
	"time"
)

func TestWindowActive(t *testing.T) {
	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)
	cases := []struct {
		name string
		w    Window
		want bool
	}{
		{"zero value", Window{}, false},
		{"consumption and future reset", Window{UsedPercent: 10, ResetsAt: future}, true},
		{"rounded zero usage with future reset", Window{UsedPercent: 0, ResetsAt: future}, true},
		{"already reset", Window{UsedPercent: 10, ResetsAt: past}, false},
		{"consumption but no reset time", Window{UsedPercent: 10}, false},
	}
	for _, c := range cases {
		if got := c.w.Active(); got != c.want {
			t.Errorf("%s: Active() = %t, want %t", c.name, got, c.want)
		}
	}
}

func TestWindowRemaining(t *testing.T) {
	if got := (Window{}).Remaining(); got != 0 {
		t.Errorf("zero window Remaining() = %v, want 0", got)
	}
	if got := (Window{ResetsAt: time.Now().Add(-time.Minute)}).Remaining(); got != 0 {
		t.Errorf("past reset Remaining() = %v, want 0 (never negative)", got)
	}
	w := Window{ResetsAt: time.Now().Add(time.Hour)}
	if got := w.Remaining(); got <= 59*time.Minute || got > time.Hour {
		t.Errorf("future reset Remaining() = %v, want ~1h", got)
	}
}

func TestWindowMissing(t *testing.T) {
	cases := []struct {
		name string
		w    Window
		want bool
	}{
		{"zero value means not enforced", Window{}, true},
		{"has usage", Window{UsedPercent: 1}, false},
		{"has reset time", Window{ResetsAt: time.Now()}, false},
		{"has window length (enforced but idle)", Window{WindowSeconds: 18000}, false},
	}
	for _, c := range cases {
		if got := c.w.Missing(); got != c.want {
			t.Errorf("%s: Missing() = %t, want %t", c.name, got, c.want)
		}
	}
}

func TestCreditsUsable(t *testing.T) {
	cases := []struct {
		name string
		u    Usage
		want bool
	}{
		{"no credits object", Usage{}, false},
		{"empty credits", Usage{Credits: &Credits{}}, false},
		{"has credits", Usage{Credits: &Credits{HasCredits: true}}, true},
		{"unlimited", Usage{Credits: &Credits{Unlimited: true}}, true},
	}
	for _, c := range cases {
		if got := c.u.CreditsUsable(); got != c.want {
			t.Errorf("%s: CreditsUsable() = %t, want %t", c.name, got, c.want)
		}
	}
}

func TestWeeklyExhausted(t *testing.T) {
	cases := []struct {
		name      string
		u         Usage
		threshold float64
		want      bool
	}{
		{"below threshold", Usage{Weekly: Window{UsedPercent: 80}}, 0.99, false},
		{"at threshold", Usage{Weekly: Window{UsedPercent: 99}}, 0.99, true},
		{"above threshold", Usage{Weekly: Window{UsedPercent: 100}}, 0.99, true},
		{"credits bypass the cap", Usage{
			Weekly:  Window{UsedPercent: 100},
			Credits: &Credits{HasCredits: true},
		}, 0.99, false},
		{"missing weekly window is not exhausted", Usage{}, 0.99, false},
	}
	for _, c := range cases {
		if got := c.u.WeeklyExhausted(c.threshold); got != c.want {
			t.Errorf("%s: WeeklyExhausted(%v) = %t, want %t", c.name, c.threshold, got, c.want)
		}
	}
}

func TestResetCreditToRedeem(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	credit := func(expiresIn time.Duration) ResetCredit {
		return ResetCredit{Status: "available", ExpiresAt: now.Add(expiresIn)}
	}
	usageWith := func(weeklyPct float64, credits ...ResetCredit) *Usage {
		return &Usage{
			Weekly:       Window{UsedPercent: weeklyPct},
			ResetCredits: &ResetCredits{AvailableCount: len(credits), Credits: credits},
		}
	}

	cases := []struct {
		name string
		u    *Usage
		want bool
	}{
		{"no credits at all", &Usage{}, false},
		{"plenty of lifetime left", usageWith(90, credit(10*24*time.Hour)), false},
		{"expiring soon but nothing to reclaim", usageWith(10, credit(6*time.Hour)), false},
		{"expiring soon with usage to reclaim", usageWith(60, credit(6*time.Hour)), true},
		{"last hour reclaims unconditionally", usageWith(1, credit(30*time.Minute)), true},
		{"already expired", usageWith(90, credit(-time.Minute)), false},
		{"already redeemed", usageWith(90, ResetCredit{
			Status: "redeemed", ExpiresAt: now.Add(time.Minute), RedeemedAt: now.Add(-time.Hour),
		}), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, got := tc.u.ResetCreditToRedeem(now); got != tc.want {
				t.Fatalf("ResetCreditToRedeem() = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestResetCreditToRedeemPicksSoonestExpiring(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	soonest := ResetCredit{Status: "available", ExpiresAt: now.Add(2 * time.Hour)}
	u := &Usage{
		Weekly: Window{UsedPercent: 80},
		ResetCredits: &ResetCredits{Credits: []ResetCredit{
			{Status: "available", ExpiresAt: now.Add(20 * time.Hour)},
			soonest,
			{Status: "expired", ExpiresAt: now.Add(-time.Hour)},
		}},
	}
	got, ok := u.ResetCreditToRedeem(now)
	if !ok || !got.ExpiresAt.Equal(soonest.ExpiresAt) {
		t.Fatalf("ResetCreditToRedeem() = %v/%t, want the credit expiring at %v", got.ExpiresAt, ok, soonest.ExpiresAt)
	}
}
