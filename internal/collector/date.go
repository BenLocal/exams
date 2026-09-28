package collector

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// dateRe matches the date formats Chinese government sites actually print:
// 2026-09-23, 2026/09/23, 2026.09.23, 2026年9月23日.
//
// The trailing 日 is part of the match so that the raw text preserved in
// published_raw is the whole date as printed, not a truncated "2026年9月23".
//
// A custom pattern passed to parseDate must expose the same three capture
// groups: year, month, day.
var dateRe = regexp.MustCompile(`(\d{4})\s*[-年/.]\s*(\d{1,2})\s*[-月/.]\s*(\d{1,2})\s*日?`)

// ParseDate extracts the first date from s and returns it along with the exact
// text that matched.
//
// Exported for script-based collectors, which should not have to reimplement
// Chinese date parsing: they pass the raw text from the page and get back both
// a time and the substring it came from.
func ParseDate(s string, loc *time.Location) (*time.Time, string) {
	return parseDate(s, nil, loc)
}

// parseDate extracts the first date from s and returns it along with the exact
// text that matched.
//
// The raw match is returned so it can be stored verbatim: an ambiguous parse
// must never destroy what the source actually printed.
func parseDate(s string, re *regexp.Regexp, loc *time.Location) (*time.Time, string) {
	if re == nil {
		re = dateRe
	}
	m := re.FindStringSubmatch(s)
	if m == nil || len(m) < 4 {
		return nil, ""
	}
	year, err1 := strconv.Atoi(m[1])
	month, err2 := strconv.Atoi(m[2])
	day, err3 := strconv.Atoi(m[3])
	if err1 != nil || err2 != nil || err3 != nil {
		return nil, ""
	}
	if month < 1 || month > 12 || day < 1 || day > 31 {
		return nil, ""
	}
	// Many announcements carry a date with no time. Anchoring at 09:00 local
	// rather than midnight keeps the date from sliding backwards when the
	// value is later rendered or converted.
	t := time.Date(year, time.Month(month), day, 9, 0, 0, 0, loc)
	return &t, strings.TrimSpace(m[0])
}
