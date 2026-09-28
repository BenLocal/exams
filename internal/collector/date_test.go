package collector

import (
	"testing"
	"time"
)

func TestParseDateFormatsSeenOnChineseGovSites(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)

	cases := []struct {
		in   string
		want string // formatted, or "" when nothing should parse
	}{
		{"2026-09-23", "2026-09-23"},
		{"2026/09/23", "2026-09-23"},
		{"2026.09.23", "2026-09-23"},
		{"2026年9月23日", "2026-09-23"},
		{"2026年09月23日", "2026-09-23"},
		{"发布时间：2026-09-23", "2026-09-23"},
		{"发布 2026年9月23日 10:30", "2026-09-23"},
		{"[2026-02-03]", "2026-02-03"},
		{"2026-13-45", ""}, // out of range
		{"没有日期", ""},
		{"", ""},
		{"2026", ""},
	}

	for _, tc := range cases {
		got, raw := parseDate(tc.in, nil, loc)
		if tc.want == "" {
			if got != nil {
				t.Errorf("parseDate(%q) = %v, want nil", tc.in, got)
			}
			continue
		}
		if got == nil {
			t.Errorf("parseDate(%q) = nil, want %s", tc.in, tc.want)
			continue
		}
		if formatted := got.Format("2006-01-02"); formatted != tc.want {
			t.Errorf("parseDate(%q) = %s, want %s", tc.in, formatted, tc.want)
		}
		if raw == "" {
			t.Errorf("parseDate(%q) returned an empty raw match; the original "+
				"date text must be preserved", tc.in)
		}
	}
}

func TestParseDateAnchorsAtNineAMLocal(t *testing.T) {
	// Announcements almost always carry a date with no time. Anchoring at
	// midnight would let the date slide backwards once converted to UTC.
	loc := time.FixedZone("CST", 8*3600)
	got, _ := parseDate("2026-09-23", nil, loc)
	if got == nil {
		t.Fatal("expected a date")
	}
	if h := got.Hour(); h != 9 {
		t.Errorf("hour = %d, want 9 so the local date survives conversion", h)
	}
	if utc := got.UTC(); utc.Day() != 23 {
		t.Errorf("UTC day = %d, want 23 (09:00+08:00 is 01:00Z)", utc.Day())
	}
}

func TestParseDatePreservesRawText(t *testing.T) {
	loc := time.UTC
	_, raw := parseDate("公示时间：2026年9月23日 至 2026年10月8日", nil, loc)
	if raw != "2026年9月23日" {
		t.Errorf("raw = %q, want the exact matched text %q", raw, "2026年9月23日")
	}
}
