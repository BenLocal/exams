package model

import (
	"strings"
	"testing"
	"time"
)

func TestNormalizeTextCollapsesWhitespace(t *testing.T) {
	cases := map[string]string{
		"  a   b  ":           "a b",
		"a b":                 "a b", // non-breaking space
		"a　b":                 "a b", // ideographic space
		"line1\r\nline2":      "line1 line2",
		"tab\there":           "tab here",
		"\n\n  trimmed  \n\n": "trimmed",
		"":                    "",
	}
	for in, want := range cases {
		if got := NormalizeText(in); got != want {
			t.Errorf("NormalizeText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestListHashIgnoresBody(t *testing.T) {
	base := Item{Title: "标题", URL: "https://example.com/a", Summary: "摘要"}

	// The body must not influence the list hash: it decides whether the detail
	// page needs fetching, and it is computed before the body is known.
	withBody := base
	withBody.Content = "很长的正文"

	if base.ListHash() != withBody.ListHash() {
		t.Error("ListHash changed when only Content changed; it must not")
	}
	if base.BodyHash() == withBody.BodyHash() {
		t.Error("BodyHash did not change when Content changed")
	}
}

func TestBodyHashIgnoresWhitespaceOnlyChanges(t *testing.T) {
	a := Item{Content: "正文内容"}
	b := Item{Content: "  正文内容\n\n"}

	if a.BodyHash() != b.BodyHash() {
		t.Error("BodyHash must ignore whitespace-only differences, " +
			"otherwise every crawl reports a false update")
	}
}

func TestHashFieldsCannotCollideAcrossBoundaries(t *testing.T) {
	// Length-prefixing exists so that ("ab","c") and ("a","bc") hash
	// differently; without it a title/summary split could collide.
	a := Item{Title: "ab", Summary: "c"}
	b := Item{Title: "a", Summary: "bc"}

	if a.ListHash() == b.ListHash() {
		t.Error("field boundary collision: ListHash must distinguish field splits")
	}
}

func TestListHashIncludesTimes(t *testing.T) {
	t1 := time.Date(2026, 9, 23, 9, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)

	a := Item{Title: "x", PublishedAt: &t1}
	b := Item{Title: "x", PublishedAt: &t2}
	if a.ListHash() == b.ListHash() {
		t.Error("ListHash must change when the publication date changes")
	}

	// nil and zero must be distinguishable from a real date.
	c := Item{Title: "x"}
	if a.ListHash() == c.ListHash() {
		t.Error("ListHash must distinguish a nil date from a set one")
	}
}

func TestListHashIsStable(t *testing.T) {
	it := Item{Title: "标题", URL: "https://example.com/a", Summary: "摘要", Category: "考研"}
	first := it.ListHash()
	for i := 0; i < 5; i++ {
		if got := it.ListHash(); got != first {
			t.Fatalf("ListHash is not deterministic: %q != %q", got, first)
		}
	}
	if !strings.HasPrefix(first, "") || len(first) != 64 {
		t.Errorf("expected a 64-char hex sha256, got %q", first)
	}
}

func TestFormatTime(t *testing.T) {
	if got := FormatTime(nil); got != "—" {
		t.Errorf("FormatTime(nil) = %q, want a dash", got)
	}
	zero := time.Time{}
	if got := FormatTime(&zero); got != "—" {
		t.Errorf("FormatTime(zero) = %q, want a dash", got)
	}
	when := time.Date(2026, 9, 23, 14, 5, 0, 0, time.UTC)
	if got := FormatTime(&when); got != "2026-09-23 14:05" {
		t.Errorf("FormatTime = %q", got)
	}
}
