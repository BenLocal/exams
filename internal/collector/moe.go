package collector

import (
	"context"
	"fmt"
	"regexp"

	"github.com/BenLocal/exams/internal/model"
)

func init() { Register(moeSource{}) }

// moeSource crawls 中华人民共和国教育部 (moe.gov.cn) 公告公示.
//
// The list is rendered client-side by the TRS WCM CMS: the page HTML contains
// only pagination scaffolding, and the announcements themselves are fetched
// from /was5/web/search. Driving a headless browser for this would add a large
// dependency and a browser to keep running, so this instead reads the two
// channel ids the page embeds and calls the same endpoint the page's own
// JavaScript calls. That endpoint returns plain HTML fragments.
type moeSource struct{}

const (
	moeBaseURL  = "http://www.moe.gov.cn"
	moeListPage = moeBaseURL + "/jyb_xxgk/s5743/s5744/"
	// How many list pages to read on each crawl.
	moeListPages = 3
)

func (moeSource) Key() string     { return "moe" }
func (moeSource) Name() string    { return "教育部 · 公告公示" }
func (moeSource) BaseURL() string { return moeBaseURL }

// moeChannelRe pulls the channel identifiers out of the page's inline script:
//
//	var wcmid = 5744; var wasid = 254874; var recordCount = 276; ...
//
// Reading them rather than hardcoding keeps the collector working when the
// CMS is re-published with new ids.
var moeChannelRe = regexp.MustCompile(`wcmid\s*=\s*(\d+)\s*;\s*var\s+wasid\s*=\s*(\d+)`)

func (moeSource) List(ctx context.Context, f *Fetcher) ([]model.Item, error) {
	doc, err := f.Get(ctx, moeListPage)
	if err != nil {
		return nil, fmt.Errorf("moe: %w", err)
	}
	pageHTML, err := doc.Html()
	if err != nil {
		return nil, fmt.Errorf("moe: read list page: %w", err)
	}

	m := moeChannelRe.FindStringSubmatch(pageHTML)
	if m == nil {
		return nil, fmt.Errorf(
			"moe: could not find the channel ids on %s\n"+
				"  The CMS page layout has changed. Open the page, find\n"+
				"  `var wcmid = ...; var wasid = ...`, and update moeChannelRe in\n"+
				"  internal/collector/moe.go, then re-run `exams probe moe`", moeListPage)
	}
	// m[1] is the channel id used as chnlid, m[2] the one used as channelid.
	wcmid, wasid := m[1], m[2]

	pages := make([]string, 0, moeListPages)
	for p := 1; p <= moeListPages; p++ {
		pages = append(pages, fmt.Sprintf(
			"%s/was5/web/search?channelid=%s&chnlid=%s&page=%d",
			moeBaseURL, wasid, wcmid, p))
	}

	return extractList(ctx, f, moeSource{}.Key(), listPageConfig{
		// The endpoint returns a bare sequence of <li> items, so no container
		// scoping is needed. Rows without an href are skipped by the parser.
		ItemSelector: "li",
		LinkSelector: "a",
		DateSelector: "span",
		Region:       "全国",
		Pages:        pages,
	})
}

// moeContentSelectors are the article body containers, tried in order.
//
// Several are needed: the 公告公示 section under /jyb_xxgk/ uses .TRS_Editor,
// while documents republished from /srcsite/ use a different template without
// it. A single selector silently fails on the whole of the second group.
var moeContentSelectors = []string{
	".TRS_Editor",
	"#content_body_xxgk",
	".moe-detail-box",
	"div.article-content",
	"#xxgk_content",
}

// Detail fetches the announcement body.
func (moeSource) Detail(ctx context.Context, f *Fetcher, it model.Item) (model.Item, error) {
	body, _, err := extractBody(ctx, f, it.URL, moeContentSelectors...)
	if err != nil {
		return it, fmt.Errorf("moe detail %s: %w", it.URL, err)
	}
	it.Content = body
	return it, nil
}
