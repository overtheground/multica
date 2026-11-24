package handler

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestSessionDropped(t *testing.T) {
	ctxOf := func(n int64, ok bool) func() (int64, bool) {
		return func() (int64, bool) { return n, ok }
	}
	capOf := func(n int64) pgtype.Int8 { return pgtype.Int8{Int64: n, Valid: true} }
	tests := []struct {
		name  string
		fresh bool
		cap   pgtype.Int8
		last  func() (int64, bool)
		want  bool
	}{
		{"default keeps the session", false, pgtype.Int8{}, ctxOf(900000, true), false},
		{"fresh per run drops it", true, pgtype.Int8{}, ctxOf(0, false), true},
		{"below cap keeps it", false, capOf(300000), ctxOf(299999, true), false},
		{"at cap keeps it", false, capOf(300000), ctxOf(300000, true), false},
		{"above cap drops it", false, capOf(300000), ctxOf(300001, true), true},
		{"cap without a report keeps it", false, capOf(300000), ctxOf(0, false), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionDropped(tc.fresh, tc.cap, tc.last); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
