package service

import (
	"testing"
	"time"
)

func TestNotificationDecisionAwareness(t *testing.T) {
	now := time.Date(2026, 10, 27, 17, 21, 36, 0, time.UTC)
	f := NotificationFacts{Subject: "claude/account-1/five_hour", Period: "reset-21:00", Action: "blocked_until_reset", ObservedAt: now.Add(-time.Minute), ResetAt: now.Add(4 * time.Hour), FreshUntil: now.Add(time.Minute), Urgency: "routine"}
	a := NotificationAwareness{Subject: f.Subject, Period: f.Period, Action: f.Action, OccurredAt: now.Add(-19 * time.Second), ExpiresAt: f.ResetAt, MessageID: "verified-member-message"}
	cases := []struct {
		name    string
		change  func(*NotificationFacts, *NotificationAwareness)
		verdict NotificationVerdict
		reason  string
	}{
		{"queued_before_ack", func(*NotificationFacts, *NotificationAwareness) {}, NotificationSuppress, "ack_same_period_and_action"},
		{"new_period", func(f *NotificationFacts, _ *NotificationAwareness) { f.Period = "next" }, NotificationSend, "new_or_unacknowledged_action"},
		{"independent_window", func(f *NotificationFacts, _ *NotificationAwareness) { f.Subject = "claude/account-1/seven_day" }, NotificationSend, "new_or_unacknowledged_action"},
		{"different_account", func(f *NotificationFacts, _ *NotificationAwareness) { f.Subject = "claude/account-2/five_hour" }, NotificationSend, "new_or_unacknowledged_action"},
		{"reset_delayed", func(f *NotificationFacts, _ *NotificationAwareness) { f.Action = "reset_delayed" }, NotificationSend, "new_or_unacknowledged_action"},
		{"new_blocker", func(f *NotificationFacts, _ *NotificationAwareness) { f.Action = "promised_access_missing" }, NotificationSend, "new_or_unacknowledged_action"},
		{"stale_source", func(f *NotificationFacts, _ *NotificationAwareness) { f.FreshUntil = now }, NotificationSend, "source_uncertain_use_historical_wording"},
		{"reset_passed", func(f *NotificationFacts, _ *NotificationAwareness) { f.ResetAt = now }, NotificationSend, "source_uncertain_use_historical_wording"},
		{"unknown_risk", func(f *NotificationFacts, _ *NotificationAwareness) { f.Urgency = "unknown" }, NotificationSend, "urgent_or_unknown_risk"},
		{"explicit_reminder", func(f *NotificationFacts, _ *NotificationAwareness) { f.ExplicitReminder = true }, NotificationSend, "explicit_schedule"},
		{"expired_ack", func(_ *NotificationFacts, a *NotificationAwareness) { a.ExpiresAt = now }, NotificationSend, "new_or_unacknowledged_action"},
		{"future_ack", func(_ *NotificationFacts, a *NotificationAwareness) { a.OccurredAt = now.Add(time.Second) }, NotificationSend, "new_or_unacknowledged_action"},
		{"unbounded_ack", func(_ *NotificationFacts, a *NotificationAwareness) { a.ExpiresAt = f.ResetAt.Add(time.Hour) }, NotificationSend, "new_or_unacknowledged_action"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			facts, ack := f, a
			tc.change(&facts, &ack)
			got := DecideNotification(facts, &ack, false, time.Time{}, now)
			if got.Verdict != tc.verdict || got.Reason != tc.reason {
				t.Fatalf("got %+v", got)
			}
			if got.Verdict == NotificationSuppress && got.AckMessageID != a.MessageID {
				t.Fatal("missing provenance")
			}
		})
	}
	if d := DecideNotification(f, nil, false, time.Time{}, now); d.Verdict != NotificationSend {
		t.Fatalf("no ack: %+v", d)
	}
}

func TestNotificationDecisionStoreFailureHasBoundedDefer(t *testing.T) {
	now := time.Now().UTC()
	f := NotificationFacts{Urgency: "routine"}
	d := DecideNotification(f, nil, true, time.Time{}, now)
	if d.Verdict != NotificationDefer || d.RetryAt == nil || !d.RetryAt.Equal(now.Add(30*time.Second)) {
		t.Fatalf("first failure: %+v", d)
	}
	d = DecideNotification(f, nil, true, now.Add(-time.Minute), now)
	if d.Verdict != NotificationSend || d.Reason != "ack_store_unavailable_fail_open" {
		t.Fatalf("budget expired: %+v", d)
	}
	f.ExplicitReminder = true
	if d = DecideNotification(f, nil, true, time.Time{}, now); d.Verdict != NotificationSend {
		t.Fatalf("schedule blocked: %+v", d)
	}
}
