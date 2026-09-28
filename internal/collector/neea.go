package collector

import (
	"context"
	"fmt"

	"github.com/BenLocal/exams/internal/model"
)

func init() { Register(neeaSource{}) }

// neeaSource crawls 中国教育考试网 (neea.edu.cn) 公示公告.
//
// Selectors verified against the live site. The list markup is:
//
//	<div class="conlistn"><ul>
//	  <li><span id='ReportIDname'><a href='/html1/report/2609/27-1.htm'
//	        title="…">标题</a></span>
//	      <span id='ReportIDIssueTime'>2026-09-04</span></li>
//
// Scoping to div.conlistn matters: the page has several other <ul> elements
// (site nav, provincial exam-board links) that a bare `ul li` would sweep in.
//
// If the site is restyled, `exams probe neea --dump-html ./tmp/neea` saves the
// live page so the selector can be corrected against the real markup.
type neeaSource struct{}

const neeaBaseURL = "https://www.neea.edu.cn"

func (neeaSource) Key() string     { return "neea" }
func (neeaSource) Name() string    { return "中国教育考试网 · 公示公告" }
func (neeaSource) BaseURL() string { return neeaBaseURL }

var neeaList = listPageConfig{
	// Scoped to rows carrying the report-title span: the pagination list also
	// lives inside div.conlistn, and a bare `ul li` pulls its page links in as
	// if they were announcements.
	ItemSelector: "div.conlistn ul li:has(span#ReportIDname)",
	LinkSelector: "a",
	DateSelector: "span#ReportIDIssueTime",
	Region:       "全国",
	// The section paginates as 151-N.htm. Three pages is roughly a month of
	// announcements, which is more than enough for a "what's new" tool and
	// keeps the steady-state crawl to three list requests.
	Pages: []string{
		neeaBaseURL + "/html1/category/1508/151-1.htm",
		neeaBaseURL + "/html1/category/1508/151-2.htm",
		neeaBaseURL + "/html1/category/1508/151-3.htm",
	},
}

func (neeaSource) List(ctx context.Context, f *Fetcher) ([]model.Item, error) {
	return extractList(ctx, f, neeaSource{}.Key(), neeaList)
}

// neeaContentSelectors are the article body containers, tried in order.
// Verified against live detail pages with `exams probe neea --with-detail`.
var neeaContentSelectors = []string{"div.conts", "div#content_body", ".TRS_Editor"}

// Detail fetches the announcement body. It is only called for items that are
// new or whose list-level fields changed.
func (neeaSource) Detail(ctx context.Context, f *Fetcher, it model.Item) (model.Item, error) {
	body, _, err := extractBody(ctx, f, it.URL, neeaContentSelectors...)
	if err != nil {
		return it, fmt.Errorf("neea detail %s: %w", it.URL, err)
	}
	it.Content = body
	return it, nil
}
