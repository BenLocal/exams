package collector

import (
	"context"
	"fmt"

	"github.com/BenLocal/exams/internal/model"
)

func init() { Register(cptaSource{}) }

// cptaSource crawls 中国人事考试网 (cpta.com.cn) 通知公告.
//
// This is the professional-qualification counterpart to neea: 建造师、经济师、
// 执业药师 and the rest of the 专业技术人员职业资格考试 calendar live here.
//
// Selectors verified against the live site:
//
//	<ul class="list_14">
//	  <li><a href="/notice/2128.html" title="完整标题">被截断的标题…</a>
//	      <i class="red">[2026-02-03]</i></li>
//
// The link text is truncated with an ellipsis and the full title lives in the
// title attribute; itemFromRow prefers whichever is longer, so no special
// handling is needed here.
type cptaSource struct{}

const cptaBaseURL = "http://www.cpta.com.cn"

func (cptaSource) Key() string     { return "cpta" }
func (cptaSource) Name() string    { return "中国人事考试网 · 通知公告" }
func (cptaSource) BaseURL() string { return cptaBaseURL }

var cptaList = listPageConfig{
	ItemSelector: "ul.list_14 li",
	LinkSelector: "a",
	DateSelector: "i",
	Region:       "全国",
	// The section paginates as notice.html, notice_2.html, notice_3.html…
	Pages: []string{
		cptaBaseURL + "/notice.html",
		cptaBaseURL + "/notice_2.html",
		cptaBaseURL + "/notice_3.html",
	},
}

func (cptaSource) List(ctx context.Context, f *Fetcher) ([]model.Item, error) {
	return extractList(ctx, f, cptaSource{}.Key(), cptaList)
}

// cptaContentSelectors are the article body containers, tried in order.
// Note that p_content is an id, not a class:
//
//	<div id="p_content"><p>…</p></div>
var cptaContentSelectors = []string{"div#p_content", "div.p_content", "div.text_con"}

// Detail fetches the announcement body.
func (cptaSource) Detail(ctx context.Context, f *Fetcher, it model.Item) (model.Item, error) {
	body, _, err := extractBody(ctx, f, it.URL, cptaContentSelectors...)
	if err != nil {
		return it, fmt.Errorf("cpta detail %s: %w", it.URL, err)
	}
	it.Content = body
	return it, nil
}
