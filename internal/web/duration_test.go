package web

import "testing"

func TestFormatMsDur(t *testing.T) {
	cases := []struct {
		ms   int
		want string
	}{
		{0, "0s"},
		{-1, "0s"},
		{1000, "1s"},
		{45_000, "45s"},
		{900_000, "15m"},
		{1_800_000, "30m"},
		{3_600_000, "1h"},
		{7_200_000, "2h"},
		{5_400_000, "90m"}, // 1.5h → compact minutes (exact minute)
		{1500, "1500ms"},
		{500, "500ms"},
	}
	for _, tc := range cases {
		if got := formatMsDur(tc.ms); got != tc.want {
			t.Errorf("formatMsDur(%d)=%q want %q", tc.ms, got, tc.want)
		}
	}
}

func TestParseTimeoutMs(t *testing.T) {
	ok := []struct {
		in   string
		want int
	}{
		{"1h", 3_600_000},
		{"30m", 1_800_000},
		{"15m", 900_000},
		{"45s", 45_000},
		{"1h30m", 5_400_000},
		{"90m", 5_400_000},
		{"1200000", 1_200_000}, // bare ms (legacy)
		{"  1h  ", 3_600_000},
		{"900000ms", 900_000},
		{"1.5h", 5_400_000},
	}
	for _, tc := range ok {
		got, err := parseTimeoutMs(tc.in)
		if err != nil {
			t.Errorf("parseTimeoutMs(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseTimeoutMs(%q)=%d want %d", tc.in, got, tc.want)
		}
	}

	bad := []string{"", "   ", "abc", "1 hour", "-5m", "1x"}
	for _, in := range bad {
		if _, err := parseTimeoutMs(in); err == nil {
			t.Errorf("parseTimeoutMs(%q): want error", in)
		}
	}
}

func TestParseTimeoutMsRoundTrip(t *testing.T) {
	for _, ms := range []int{1000, 45_000, 900_000, 1_800_000, 3_600_000, 5_400_000, 1500} {
		s := formatMsDur(ms)
		got, err := parseTimeoutMs(s)
		if err != nil {
			t.Fatalf("round-trip %d → %q: %v", ms, s, err)
		}
		if got != ms {
			t.Fatalf("round-trip %d → %q → %d", ms, s, got)
		}
	}
}
