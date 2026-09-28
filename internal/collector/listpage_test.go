package collector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BenLocal/exams/internal/model"
)

func testFetcher(t *testing.T) *Fetcher {
	t.Helper()
	f, err := NewFetcher("test-agent", "", 10*time.Second, time.FixedZone("CST", 8*3600))
	if err != nil {
		t.Fatalf("NewFetcher: %v", err)
	}
	return f
}

// serveHTML starts a test server returning the given body for every path.
func serveHTML(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

const sampleList = `<!doctype html><html><body>
<div class="conlistn">
  <ul>
    <li><span id='ReportIDname'><a href='/html1/report/2609/27-1.htm'
        title="关于2026年度考试的公告">关于2026年度考试的公告</a></span>
        <span id='ReportIDIssueTime'>2026-09-04</span></li>
    <li><span id='ReportIDname'><a href='../report/2608/25-1.htm'>关于拟立项名单的公示</a></span>
        <span id='ReportIDIssueTime'>2026-08-26</span></li>
    <li><span>这一行没有链接，必须被跳过</span></li>
  </ul>
</div>
<ul class="page"><li><a href='/html1/category/1508/151-2.htm'>2</a></li></ul>
</body></html>`

func TestExtractListParsesAndResolvesURLs(t *testing.T) {
	srv := serveHTML(t, sampleList)
	f := testFetcher(t)

	items, err := extractList(context.Background(), f, "test", listPageConfig{
		ItemSelector: "div.conlistn ul li:has(span#ReportIDname)",
		DateSelector: "span#ReportIDIssueTime",
		Pages:        []string{srv.URL + "/html1/category/1508/151-1.htm"},
	})
	if err != nil {
		t.Fatalf("extractList: %v", err)
	}

	// The third row has no anchor and must be skipped, and the pagination list
	// lives outside div.conlistn.
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2: %+v", len(items), items)
	}

	first := items[0]
	if first.Title != "关于2026年度考试的公告" {
		t.Errorf("title = %q", first.Title)
	}
	// A leading-slash href resolves against the page's host.
	if !strings.HasPrefix(first.URL, srv.URL+"/html1/report/") {
		t.Errorf("URL = %q, want it resolved against %s", first.URL, srv.URL)
	}
	// A relative ../ href resolves too.
	if !strings.Contains(items[1].URL, "/report/2608/") {
		t.Errorf("relative href resolved to %q", items[1].URL)
	}
	if first.PublishedAt == nil || first.PublishedAt.Format("2006-01-02") != "2026-09-04" {
		t.Errorf("published = %v", first.PublishedAt)
	}
	if first.PublishedRaw != "2026-09-04" {
		t.Errorf("published_raw = %q, want the source's own text", first.PublishedRaw)
	}
	// Identity defaults to the canonical detail URL.
	if first.ExternalID != first.URL {
		t.Errorf("ExternalID = %q, want it to equal URL %q", first.ExternalID, first.URL)
	}
}

func TestExtractListPrefersLongerTitleAttribute(t *testing.T) {
	// 中国人事考试网 truncates the link text and keeps the full string in the
	// title attribute; the attribute must win when it is longer.
	body := `<html><body><ul class="list">
	  <li><a href="/notice/1.html" title="人力资源社会保障部办公厅关于2026年度专业技术人员职业资格考试工作计划及有关事项的通知">人力资源社会保障部办公厅关于2026年度专业技术人员职业资格考试工...</a>
	      <i>[2026-02-03]</i></li>
	</ul></body></html>`
	srv := serveHTML(t, body)

	items, err := extractList(context.Background(), testFetcher(t), "test", listPageConfig{
		ItemSelector: "ul.list li",
		DateSelector: "i",
		Pages:        []string{srv.URL + "/notice.html"},
	})
	if err != nil {
		t.Fatalf("extractList: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	if !strings.HasSuffix(items[0].Title, "及有关事项的通知") {
		t.Errorf("title = %q, want the full title attribute, not the truncated text", items[0].Title)
	}
}

func TestExtractListErrorsWhenSelectorMatchesNothing(t *testing.T) {
	// The failure this guards against is the dangerous one: a restyled site
	// that yields zero items and would otherwise be recorded as a quiet news
	// day. The error must name the selector so it can be fixed directly.
	srv := serveHTML(t, `<html><body><div class="totally-different">x</div></body></html>`)

	_, err := extractList(context.Background(), testFetcher(t), "demo-key", listPageConfig{
		ItemSelector: "div.conlistn ul li",
		Pages:        []string{srv.URL + "/list.html"},
	})
	if err == nil {
		t.Fatal("expected an error when the selector matches nothing")
	}
	msg := err.Error()
	for _, want := range []string{"demo-key", "div.conlistn ul li", "exams probe demo-key"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message is missing %q; got:\n%s", want, msg)
		}
	}
}

func TestExtractListErrorsWhenRowsHaveNoUsableLink(t *testing.T) {
	srv := serveHTML(t, `<html><body><ul class="list">
	  <li><span>没有链接</span></li>
	  <li><span>也没有</span></li>
	</ul></body></html>`)

	_, err := extractList(context.Background(), testFetcher(t), "test", listPageConfig{
		ItemSelector: "ul.list li",
		Pages:        []string{srv.URL + "/list.html"},
	})
	if err == nil {
		t.Fatal("expected an error when rows yield no usable links")
	}
	if !strings.Contains(err.Error(), "none yielded a usable link") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestExtractListSkipsJavascriptAndAnchorHrefs(t *testing.T) {
	srv := serveHTML(t, `<html><body><ul class="list">
	  <li><a href="javascript:void(0)">假的</a></li>
	  <li><a href="#top">锚点</a></li>
	  <li><a href="/real.html">真的</a></li>
	</ul></body></html>`)

	items, err := extractList(context.Background(), testFetcher(t), "test", listPageConfig{
		ItemSelector: "ul.list li",
		Pages:        []string{srv.URL + "/list.html"},
	})
	if err != nil {
		t.Fatalf("extractList: %v", err)
	}
	if len(items) != 1 || !strings.HasSuffix(items[0].URL, "/real.html") {
		t.Errorf("got %+v, want only the real link", items)
	}
}

func TestExtractListDeduplicatesWithinARun(t *testing.T) {
	// The same announcement appearing on two list pages must be stored once.
	body := `<html><body><ul class="list">
	  <li><a href="/a.html">公告</a></li>
	  <li><a href="/a.html">公告</a></li>
	</ul></body></html>`
	srv := serveHTML(t, body)

	items, err := extractList(context.Background(), testFetcher(t), "test", listPageConfig{
		ItemSelector: "ul.list li",
		Pages:        []string{srv.URL + "/list.html"},
	})
	if err != nil {
		t.Fatalf("extractList: %v", err)
	}
	if len(items) != 1 {
		t.Errorf("got %d items, want 1 deduplicated", len(items))
	}
}

func TestExtractListUsesCustomDatePattern(t *testing.T) {
	srv := serveHTML(t, `<html><body><ul class="list">
	  <li><a href="/a.html">公告</a><span>发布于 09/23/2026</span></li>
	</ul></body></html>`)

	items, err := extractList(context.Background(), testFetcher(t), "test", listPageConfig{
		ItemSelector: "ul.list li",
		DateSelector: "span",
		Pages:        []string{srv.URL + "/list.html"},
	})
	if err != nil {
		t.Fatalf("extractList: %v", err)
	}
	// The default pattern only understands year-first dates, so this one is
	// legitimately unparsed — and must not be invented.
	if items[0].PublishedAt != nil {
		t.Errorf("published = %v, want nil for an unrecognised format",
			items[0].PublishedAt)
	}
	if items[0].Title != "公告" {
		t.Errorf("title = %q", items[0].Title)
	}
}

func TestResolveURL(t *testing.T) {
	base := "https://example.com/a/b/page.html"

	cases := []struct {
		href string
		want string
		bad  bool
	}{
		{"c.html", "https://example.com/a/b/c.html", false},
		{"../d.html", "https://example.com/a/d.html", false},
		{"/root.html", "https://example.com/root.html", false},
		{"//other.com/x", "https://other.com/x", false},
		{"https://third.com/y", "https://third.com/y", false},
		{"page.html#section", "https://example.com/a/b/page.html", false},
		{"page.html?utm_source=x&id=1", "https://example.com/a/b/page.html?id=1", false},
		{"javascript:void(0)", "", true},
		{"#anchor", "", true},
		{"mailto:a@b.com", "", true},
		{"   ", "", true},
	}
	for _, tc := range cases {
		got, err := ResolveURL(base, tc.href)
		if tc.bad {
			if err == nil {
				t.Errorf("ResolveURL(%q) = %q, want an error", tc.href, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ResolveURL(%q): %v", tc.href, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ResolveURL(%q) = %q, want %q", tc.href, got, tc.want)
		}
	}
}

func TestDemoSourceIsOfflineAndDeterministic(t *testing.T) {
	f := testFetcher(t)
	ctx := context.Background()

	first, err := demoSource{}.List(ctx, f)
	if err != nil {
		t.Fatalf("demo List: %v", err)
	}
	if len(first) < 20 {
		t.Fatalf("demo produced %d items, want enough to exercise pagination", len(first))
	}

	second, err := demoSource{}.List(ctx, f)
	if err != nil {
		t.Fatalf("demo List: %v", err)
	}

	// Stable identity is what makes a re-crawl update rows instead of
	// duplicating them.
	byID := map[string]model.Item{}
	for _, it := range second {
		byID[it.ExternalID] = it
	}
	for _, it := range first {
		other, ok := byID[it.ExternalID]
		if !ok {
			t.Fatalf("external id %q disappeared between runs", it.ExternalID)
		}
		if it.ListHash() != other.ListHash() {
			t.Errorf("item %q is not stable across runs", it.ExternalID)
		}
	}
}

func TestDemoSourceCancelsOnContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := (demoSource{}).List(ctx, testFetcher(t)); err == nil {
		t.Error("expected a cancelled context to abort the demo source")
	}
}
