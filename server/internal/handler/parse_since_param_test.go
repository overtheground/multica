package handler

import (
	"net/http/httptest"
	"testing"
	"time"
)

// TestSinceFromDays pins the DST-boundary behaviour of the pure core that
// parseSinceParamInTZ delegates to. time.Now() is injected as `now`, so the
// maths can be exercised at exact transition dates without waiting for a
// real DST change. The invariant: the returned instant is genuine local
// midnight (00:00:00) of the calendar day `days` days before `now`'s local
// day — even when the subtracted span crosses a spring-forward (a 23h day)
// or fall-back (a 25h day).
func TestSinceFromDays(t *testing.T) {
	mustLoad := func(name string) *time.Location {
		loc, err := time.LoadLocation(name)
		if err != nil {
			t.Fatalf("load %s: %v", name, err)
		}
		return loc
	}
	ny := mustLoad("America/New_York")
	la := mustLoad("America/Los_Angeles")

	cases := []struct {
		name    string
		loc     *time.Location
		now     time.Time
		days    int
		wantYMD [3]int // year, month, day of expected local midnight
	}{
		{
			// NY spring-forward is 2025-03-09 (clocks jump 02:00→03:00).
			// From 03-12, days=3 lands on 03-09 local midnight, and the
			// 3-day span swallows the 23-hour DST day.
			name:    "NY spring-forward span",
			loc:     ny,
			now:     time.Date(2025, 3, 12, 15, 30, 0, 0, ny),
			days:    3,
			wantYMD: [3]int{2025, 3, 9},
		},
		{
			// Land exactly ON the spring-forward day.
			name:    "NY spring-forward day itself",
			loc:     ny,
			now:     time.Date(2025, 3, 9, 10, 0, 0, 0, ny),
			days:    0,
			wantYMD: [3]int{2025, 3, 9},
		},
		{
			// NY fall-back is 2025-11-02 (clocks fall 02:00→01:00, 25h day).
			name:    "NY fall-back span",
			loc:     ny,
			now:     time.Date(2025, 11, 5, 8, 0, 0, 0, ny),
			days:    4,
			wantYMD: [3]int{2025, 11, 1},
		},
		{
			name:    "NY fall-back day itself",
			loc:     ny,
			now:     time.Date(2025, 11, 2, 23, 59, 0, 0, ny),
			days:    0,
			wantYMD: [3]int{2025, 11, 2},
		},
		{
			// LA spring-forward also 2025-03-09.
			name:    "LA spring-forward span",
			loc:     la,
			now:     time.Date(2025, 3, 10, 6, 15, 0, 0, la),
			days:    2,
			wantYMD: [3]int{2025, 3, 8},
		},
		{
			// LA fall-back also 2025-11-02.
			name:    "LA fall-back span",
			loc:     la,
			now:     time.Date(2025, 11, 3, 0, 30, 0, 0, la),
			days:    5,
			wantYMD: [3]int{2025, 10, 29},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := sinceFromDays(tc.now, tc.days, tc.loc)

			// Result must be genuine local midnight in tc.loc.
			y, m, d := got.In(tc.loc).Date()
			hh, mm, ss := got.In(tc.loc).Clock()
			if hh != 0 || mm != 0 || ss != 0 {
				t.Errorf("not local midnight: got %s in %s", got.In(tc.loc).Format(time.RFC3339), tc.loc)
			}
			if y != tc.wantYMD[0] || int(m) != tc.wantYMD[1] || d != tc.wantYMD[2] {
				t.Errorf("calendar day: got %04d-%02d-%02d, want %04d-%02d-%02d",
					y, m, d, tc.wantYMD[0], tc.wantYMD[1], tc.wantYMD[2])
			}

			// Cross-check: it equals time.Date midnight of the target day.
			want := time.Date(tc.wantYMD[0], time.Month(tc.wantYMD[1]), tc.wantYMD[2], 0, 0, 0, 0, tc.loc)
			if !got.Equal(want) {
				t.Errorf("instant mismatch: got %s, want %s",
					got.Format(time.RFC3339), want.Format(time.RFC3339))
			}
		})
	}
}

// TestParseSinceParamInTZ guards the HTTP-layer parsing in
// parseSinceParamInTZ — the `strconv.Atoi` + `parsed > 0 && parsed <= 365`
// validation and the no-param default. TestSinceFromDays covers the pure
// date maths; this covers how the `days` query param is read and clamped.
// The expected cutoff is recomputed via sinceFromDays, so each case asserts
// the param resolved to the day count the handler should have used.
func TestParseSinceParamInTZ(t *testing.T) {
	utc := time.UTC

	// expectDays builds the cutoff parseSinceParamInTZ must return for a
	// given effective day count, anchored to "now" at call time. The window
	// is computed against time.Now() inside parseSinceParamInTZ, so a tiny
	// skew across a day boundary is possible; cases run well clear of it.
	expectDays := func(days int) time.Time {
		return sinceFromDays(time.Now(), days, utc)
	}

	cases := []struct {
		name        string
		query       string // raw query string, no leading '?'
		defaultDays int
		wantDays    int // effective day count the cutoff must reflect
	}{
		// Invalid / out-of-range inputs all fall back to defaultDays.
		{"days=0 rejected (not > 0)", "days=0", 30, 30},
		{"days=abc rejected (not an int)", "days=abc", 30, 30},
		{"days=400 rejected (over 365 cap)", "days=400", 30, 30},
		{"days=-5 rejected (negative)", "days=-5", 30, 30},
		{"no days param uses default", "", 30, 30},
		{"empty days param uses default", "days=", 30, 30},
		// Valid inputs are honoured, including the 365 boundary.
		{"valid days=7 honoured", "days=7", 30, 7},
		{"days=365 honoured (at cap)", "days=365", 30, 365},
		{"days=1 honoured (lower bound)", "days=1", 90, 1},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			url := "/api/dashboard/usage/daily"
			if tc.query != "" {
				url += "?" + tc.query
			}
			req := httptest.NewRequest("GET", url, nil)

			got := parseSinceParamInTZ(req, tc.defaultDays, "UTC")
			if !got.Valid {
				t.Fatalf("expected a valid timestamptz, got Valid=false")
			}
			want := expectDays(tc.wantDays)
			// Allow a 2s slop for the now() skew between the two calls.
			if diff := got.Time.Sub(want); diff < -2*time.Second || diff > 2*time.Second {
				t.Errorf("cutoff mismatch: got %s, want ~%s (effective days=%d)",
					got.Time.Format(time.RFC3339), want.Format(time.RFC3339), tc.wantDays)
			}
		})
	}
}

// TestSinceFromHours pins the rolling-window cutoff: N hourly buckets ending
// at the current (partial) UTC hour, whatever zone `now` is expressed in.
func TestSinceFromHours(t *testing.T) {
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load tz: %v", err)
	}
	cases := []struct {
		name  string
		now   time.Time
		hours int
		want  time.Time
	}{
		{"24h mid-hour", time.Date(2026, 11, 16, 13, 41, 7, 0, time.UTC), 24, time.Date(2026, 11, 15, 14, 0, 0, 0, time.UTC)},
		{"1h is just the current hour", time.Date(2026, 11, 16, 13, 41, 0, 0, time.UTC), 1, time.Date(2026, 11, 16, 13, 0, 0, 0, time.UTC)},
		{"crosses midnight", time.Date(2026, 11, 16, 2, 5, 0, 0, time.UTC), 24, time.Date(2026, 11, 15, 3, 0, 0, 0, time.UTC)},
		{"viewer zone does not move the edge", time.Date(2026, 11, 16, 21, 41, 0, 0, shanghai), 24, time.Date(2026, 11, 15, 14, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		if got := sinceFromHours(c.now, c.hours); !got.Equal(c.want) {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestHoursParam(t *testing.T) {
	cases := []struct {
		query  string
		want   int
		wantOK bool
	}{
		{"", 0, false},
		{"hours=24", 24, true},
		{"hours=1", 1, true},
		{"hours=48", 48, true},
		{"hours=49", 0, false},
		{"hours=0", 0, false},
		{"hours=-3", 0, false},
		{"hours=abc", 0, false},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/api/dashboard/usage/daily?"+c.query, nil)
		got, ok := hoursParam(r)
		if got != c.want || ok != c.wantOK {
			t.Errorf("%q: got (%d,%v), want (%d,%v)", c.query, got, ok, c.want, c.wantOK)
		}
	}
}

// With ?hours the dashboard cutoff is the rolling window for both the N+1 and
// exact variants; without it the calendar ?days cutoff is untouched.
func TestDashboardSince(t *testing.T) {
	fixed := time.Date(2026, 11, 16, 13, 41, 0, 0, time.UTC)
	prev := dayWindowNow
	dayWindowNow = func() time.Time { return fixed }
	t.Cleanup(func() { dayWindowNow = prev })

	hourly := httptest.NewRequest("GET", "/x?hours=24&days=7", nil)
	want := time.Date(2026, 11, 15, 14, 0, 0, 0, time.UTC)
	for _, exact := range []bool{false, true} {
		if got := dashboardSince(hourly, 30, "UTC", exact).Time; !got.Equal(want) {
			t.Errorf("hours exact=%v: got %s, want %s", exact, got, want)
		}
	}

	daily := httptest.NewRequest("GET", "/x?days=1", nil)
	if got := dashboardSince(daily, 30, "UTC", true).Time; !got.Equal(time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("days=1 exact: got %s, want today's midnight", got)
	}
	if got := dashboardSince(daily, 30, "UTC", false).Time; !got.Equal(time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("days=1 N+1: got %s, want yesterday's midnight", got)
	}
}

func TestBucketHour(t *testing.T) {
	if bucketHour(-1) != nil {
		t.Error("-1 must map to nil")
	}
	if p := bucketHour(0); p == nil || *p != 0 {
		t.Error("0 must map to a pointer to 0")
	}
}
