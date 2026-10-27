package service

import (
	"errors"
	"strings"
	"time"
)

// NotificationVerdict describes interruption policy independently of transport.
type NotificationVerdict string

const (
	NotificationSend     NotificationVerdict = "send"
	NotificationDefer    NotificationVerdict = "defer"
	NotificationSuppress NotificationVerdict = "suppress"
)

// NotificationFacts is structured source evidence, never text extracted from an
// issue comment. Identity includes the account, window and reset period.
type NotificationFacts struct {
	Subject    string    `json:"subject"`
	Period     string    `json:"period"`
	Action     string    `json:"action"`
	ObservedAt time.Time `json:"observed_at"`
	ResetAt    time.Time `json:"reset_at"`
	FreshUntil time.Time `json:"fresh_until"`
	Urgency    string    `json:"urgency"`
	// Set only by a trusted scheduler, not by a generic producer request.
	ExplicitReminder bool `json:"explicit_reminder"`
}

type NotificationAwareness struct {
	Subject    string
	Period     string
	Action     string
	OccurredAt time.Time
	ExpiresAt  time.Time
	MessageID  string
}

type NotificationDecision struct {
	Verdict      NotificationVerdict `json:"verdict"`
	Reason       string              `json:"reason"`
	AckMessageID string              `json:"ack_message_id,omitempty"`
	RetryAt      *time.Time          `json:"retry_at,omitempty"`
}

func (f NotificationFacts) Validate(now time.Time) error {
	if strings.TrimSpace(f.Subject) == "" || strings.TrimSpace(f.Period) == "" || strings.TrimSpace(f.Action) == "" {
		return errors.New("subject, period and action are required")
	}
	if f.ObservedAt.IsZero() || f.ResetAt.IsZero() || f.FreshUntil.IsZero() || f.ObservedAt.After(now) || f.FreshUntil.Before(f.ObservedAt) {
		return errors.New("invalid source timestamps")
	}
	if f.Urgency != "routine" && f.Urgency != "urgent" && f.Urgency != "unknown" {
		return errors.New("urgency must be routine, urgent or unknown")
	}
	return nil
}

// DecideNotification is shared by capture and the final send authorization.
// A missing ack is different from a failed read. Retry budget is measured from
// the first failure, so a retry cannot postpone a notice indefinitely.
func DecideNotification(f NotificationFacts, ack *NotificationAwareness, ackReadFailed bool, firstFailure, now time.Time) NotificationDecision {
	if f.ExplicitReminder {
		return NotificationDecision{Verdict: NotificationSend, Reason: "explicit_schedule"}
	}
	if f.Urgency != "routine" {
		return NotificationDecision{Verdict: NotificationSend, Reason: "urgent_or_unknown_risk"}
	}
	if ackReadFailed {
		if !firstFailure.IsZero() && !now.Before(firstFailure.Add(time.Minute)) {
			return NotificationDecision{Verdict: NotificationSend, Reason: "ack_store_unavailable_fail_open"}
		}
		retry := now.Add(30 * time.Second)
		return NotificationDecision{Verdict: NotificationDefer, Reason: "ack_store_unavailable", RetryAt: &retry}
	}
	if !now.Before(f.FreshUntil) || !now.Before(f.ResetAt) {
		return NotificationDecision{Verdict: NotificationSend, Reason: "source_uncertain_use_historical_wording"}
	}
	if ack != nil && ack.Subject == f.Subject && ack.Period == f.Period && ack.Action == f.Action && !ack.OccurredAt.After(now) && now.Before(ack.ExpiresAt) && !ack.ExpiresAt.After(f.ResetAt) {
		return NotificationDecision{Verdict: NotificationSuppress, Reason: "ack_same_period_and_action", AckMessageID: ack.MessageID}
	}
	return NotificationDecision{Verdict: NotificationSend, Reason: "new_or_unacknowledged_action"}
}
