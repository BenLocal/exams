package collector

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/BenLocal/exams/internal/model"
	"github.com/PuerkitoBio/goquery"
)

// listPageConfig describes how to read one site's announcement list.
//
// Every field here is a guess until it is checked against the live site with
// `exams probe <key>`. Keeping the guesswork in a struct like this makes
// correcting it a one-line edit rather than a rewrite.
type listPageConfig struct {
	// ItemSelector picks one node per announcement row.
	ItemSelector string
	// LinkSelector picks the anchor inside a row. Defaults to "a".
	LinkSelector string
	// DateSelector optionally narrows the date search to a child element. When
	// empty the whole row's text is searched.
	DateSelector string
	// DatePattern overrides the default date regex. Must expose three capture
	// groups: year, month, day.
	DatePattern *regexp.Regexp
	// Category and Region are applied to every item from this page. Set them
	// per page when a site separates exams into sections.
	Category string
	Region   string
	// Pages lists the list-page URLs to read, in order.
	Pages []string
}

// extractList reads a set of list pages and returns the announcements found.
//
// It fails loudly rather than returning an empty slice when a selector stops
// matching. A silently-empty result is the single most common way a crawler
// breaks: the run looks successful, the dashboard stays green, and the data
// quietly stops arriving.
func extractList(ctx context.Context, f *Fetcher, sourceKey string, cfg listPageConfig) ([]model.Item, error) {
	linkSel := cfg.LinkSelector
	if linkSel == "" {
		linkSel = "a"
	}

	var out []model.Item
	seen := map[string]bool{}

	for _, pageURL := range cfg.Pages {
		doc, err := f.Get(ctx, pageURL)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", sourceKey, err)
		}

		rows := doc.Find(cfg.ItemSelector)
		if rows.Length() == 0 {
			return nil, fmt.Errorf(
				"%s: selector %q matched 0 nodes on %s\n"+
					"  The site markup has probably changed. Inspect the page and update\n"+
					"  listPageConfig in internal/collector/%s.go, then re-run:\n"+
					"      exams probe %s",
				sourceKey, cfg.ItemSelector, pageURL, sourceKey, sourceKey)
		}

		var matched, withLink int
		rows.Each(func(_ int, row *goquery.Selection) {
			matched++
			item, ok := itemFromRow(row, linkSel, cfg, pageURL, f)
			if !ok {
				return
			}
			withLink++
			if seen[item.ExternalID] {
				return
			}
			seen[item.ExternalID] = true
			out = append(out, item)
		})

		if withLink == 0 {
			return nil, fmt.Errorf(
				"%s: selector %q matched %d rows on %s but none yielded a usable link\n"+
					"  Check LinkSelector (%q) and the row markup, then re-run `exams probe %s`",
				sourceKey, cfg.ItemSelector, matched, pageURL, linkSel, sourceKey)
		}
	}

	return out, nil
}

// extractBody fetches a detail page and returns its body as plain text,
// together with the selector that matched.
//
// Selectors are tried in order and the first match wins, rather than being
// joined into one comma-separated CSS list: a list would match every candidate
// and .First() would return whichever happens to come first in the document,
// which is not necessarily the intended container. Real sites routinely run
// two or three article templates side by side, so a single selector silently
// fails on a minority of pages.
//
// The result is text, never HTML. Storing markup and rendering it through
// template.HTML would make every crawled site a stored-XSS vector, and
// html/template does not sanitise template.HTML — it only escapes plain
// strings.
func extractBody(ctx context.Context, f *Fetcher, pageURL string, selectors ...string) (text, matched string, err error) {
	doc, err := f.Get(ctx, pageURL)
	if err != nil {
		return "", "", err
	}
	for _, selector := range selectors {
		sel := doc.Find(selector).First()
		if sel.Length() == 0 {
			continue
		}
		text := TextWithBreaks(sel)
		if text == "" {
			continue
		}
		if len(text) > 20000 {
			text = text[:20000]
		}
		return text, selector, nil
	}
	return "", "", fmt.Errorf("none of the content selectors %v matched on %s",
		selectors, pageURL)
}

// itemFromRow converts one list row into an Item, reporting false when the row
// is not a usable announcement link.
func itemFromRow(row *goquery.Selection, linkSel string, cfg listPageConfig, pageURL string, f *Fetcher) (model.Item, bool) {
	a := row.Find(linkSel).First()
	if a.Length() == 0 {
		return model.Item{}, false
	}
	href, ok := a.Attr("href")
	if !ok || strings.TrimSpace(href) == "" {
		return model.Item{}, false
	}
	abs, err := ResolveURL(pageURL, href)
	if err != nil {
		return model.Item{}, false
	}

	title := strings.TrimSpace(a.Text())
	// Some templates truncate the visible link text and keep the full string
	// only in the title attribute (中国人事考试网 does exactly this), so a
	// longer title attribute wins. When they agree, or the attribute is a
	// short tooltip, the visible text is used.
	if attr, ok := a.Attr("title"); ok {
		if attr = strings.TrimSpace(attr); len([]rune(attr)) > len([]rune(title)) {
			title = attr
		}
	}
	if title == "" {
		return model.Item{}, false
	}

	dateSource := row.Text()
	if cfg.DateSelector != "" {
		if d := row.Find(cfg.DateSelector).First(); d.Length() > 0 {
			dateSource = d.Text()
		}
	}
	publishedAt, publishedRaw := parseDate(dateSource, cfg.DatePattern, f.Loc())

	summary := strings.TrimSpace(row.Text())
	if len(summary) > 300 {
		summary = summary[:300]
	}

	return model.Item{
		// The canonical detail URL is the identity: these sites rarely expose
		// a stable numeric id in the list markup.
		ExternalID:   abs,
		Title:        model.NormalizeText(title),
		URL:          abs,
		Summary:      model.NormalizeText(summary),
		Category:     cfg.Category,
		Region:       cfg.Region,
		PublishedAt:  publishedAt,
		PublishedRaw: publishedRaw,
	}, true
}
