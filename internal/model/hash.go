package model

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Two hashes per item, deliberately separate:
//
//	ListHash covers only what a list page exposes. It decides whether the
//	detail page needs to be fetched at all, which is what keeps a crawl at one
//	request per source instead of one per announcement.
//
//	BodyHash covers the body text alone, so "the content changed" can be
//	detected without loading every stored body to compare.
//
// Hashing normalised text rather than raw HTML is essential: a view counter, a
// sidebar date or an ad slot can change on every request, and hashing markup
// would turn those into a permanent stream of false "updated" events.

var (
	whitespaceRun = regexp.MustCompile(`\s+`)
	// Horizontal whitespace only, so paragraph breaks survive.
	horizontalRun = regexp.MustCompile(`[ \t\r\f\v]+`)
	blankRun      = regexp.MustCompile(`\n{3,}`)
	spaceAroundNL = regexp.MustCompile(` *\n *`)
)

// NormalizeBody prepares scraped body text for storage.
//
// Like NormalizeText, but paragraph breaks are kept: collapsing every newline
// turns a long announcement into one wall of text, which is what the detail
// page shows the reader.
//
// Hashing still uses NormalizeText, deliberately. A stricter normalisation for
// the fingerprint means a whitespace-only reflow does not register as a
// content change.
func NormalizeBody(s string) string {
	s = strings.ReplaceAll(s, " ", " ")
	s = strings.ReplaceAll(s, "　", " ")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = horizontalRun.ReplaceAllString(s, " ")
	s = spaceAroundNL.ReplaceAllString(s, "\n")
	s = blankRun.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// NormalizeText collapses whitespace and strips the non-breaking and
// ideographic spaces that Chinese CMS templates are full of.
func NormalizeText(s string) string {
	s = strings.ReplaceAll(s, " ", " ")
	s = strings.ReplaceAll(s, "　", " ")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.TrimSpace(whitespaceRun.ReplaceAllString(s, " "))
}

// ListHash fingerprints the fields available from a list page.
func (it Item) ListHash() string {
	h := sha256.New()
	writeField(h, NormalizeText(it.Title))
	writeField(h, strings.TrimSpace(it.URL))
	writeField(h, NormalizeText(it.Summary))
	writeField(h, NormalizeText(it.Category))
	writeField(h, NormalizeText(it.Region))
	writeField(h, timeField(it.PublishedAt))
	writeField(h, NormalizeText(it.PublishedRaw))
	writeField(h, timeField(it.DeadlineAt))
	return hex.EncodeToString(h.Sum(nil))
}

// BodyHash fingerprints the body text only.
func (it Item) BodyHash() string {
	h := sha256.New()
	writeField(h, NormalizeText(it.Content))
	return hex.EncodeToString(h.Sum(nil))
}

// writeField length-prefixes each value so that concatenation can never make
// two different items hash the same ("ab"+"c" vs "a"+"bc").
func writeField(h hash.Hash, s string) {
	_, _ = io.WriteString(h, strconv.Itoa(len(s)))
	_, _ = io.WriteString(h, ":")
	_, _ = io.WriteString(h, s)
	_, _ = io.WriteString(h, "\x00")
}

func timeField(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// FormatTime renders a timestamp for display, or a dash when absent.
func FormatTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "—"
	}
	return t.Format("2006-01-02 15:04")
}
