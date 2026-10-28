package service

import (
	"errors"
	"fmt"
	"time"
)

type QuotaWindow struct {
	RemainingPercent *float64  `json:"remaining_percent"`
	ResetAt          time.Time `json:"reset_at"`
}
type ClaudeQuotaSnapshot struct {
	FiveHour QuotaWindow `json:"five_hour"`
	SevenDay QuotaWindow `json:"seven_day"`
}

func RenderQuotaNotification(in NotificationEventInput) (string, error) {
	if in.Claude == nil {
		return "", errors.New("both Claude windows required for direct delivery")
	}
	window := func(label string, w QuotaWindow) (string, error) {
		remaining := "неизвестно"
		if w.RemainingPercent != nil {
			if *w.RemainingPercent < 0 || *w.RemainingPercent > 100 {
				return "", errors.New("invalid quota percentage")
			}
			remaining = fmt.Sprintf("%.0f%%", *w.RemainingPercent)
		}
		reset := "неизвестен"
		if !w.ResetAt.IsZero() {
			reset = w.ResetAt.UTC().Format(time.RFC3339)
		}
		return fmt.Sprintf("%s: остаток %s; сброс %s", label, remaining, reset), nil
	}
	five, err := window("Claude, 5 часов", in.Claude.FiveHour)
	if err != nil {
		return "", err
	}
	week, err := window("Claude, неделя", in.Claude.SevenDay)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Снимок лимитов от %s (UTC).\n%s\n%s\nСнимок действителен до %s. После этой даты или сброса текущее состояние требует проверки.", in.Facts.ObservedAt.UTC().Format(time.RFC3339), five, week, in.Facts.FreshUntil.UTC().Format(time.RFC3339)), nil
}
