package lua

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenLocal/exams/internal/collector"
	"github.com/BenLocal/exams/internal/model"
)

var testLoc = time.FixedZone("CST", 8*3600)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// writeScript drops a script into a temp dir and returns the directory.
func writeScript(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return dir
}

// loadOne compiles a single script and returns it.
func loadOne(t *testing.T, name, body string) *Source {
	t.Helper()
	dir := writeScript(t, name, body)
	res, err := LoadDir(dir, testLoc, testLogger())
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	for _, e := range res.Errors {
		t.Fatalf("script did not compile: %v", e)
	}
	if len(res.Sources) != 1 {
		t.Fatalf("got %d sources, want 1", len(res.Sources))
	}
	return res.Sources[0].(*Source)
}

func testFetcher(t *testing.T) *collector.Fetcher {
	t.Helper()
	f, err := collector.NewFetcher("test-agent", "", 10*time.Second, testLoc)
	if err != nil {
		t.Fatalf("NewFetcher: %v", err)
	}
	return f
}

// serveList starts a server that answers the list page and a detail page.
func serveList(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch {
		case strings.HasPrefix(r.URL.Path, "/detail/"):
			_, _ = w.Write([]byte(`<html><body><div class="article">
				<p>第一段正文。</p><p>第二段正文。</p></div></body></html>`))
		default:
			_, _ = w.Write([]byte(`<html><body><ul class="list">
				<li><a href="/detail/1.html">成人高考报名通知</a><span class="time">2026-09-04</span></li>
				<li><a href="/detail/2.html">自学考试安排通知</a><span class="time">2026-08-24</span></li>
				</ul></body></html>`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// listScript is a working collector, used as the base for most tests.
func listScript(baseURL string) string {
	return `
local source = {
  key = "test_src",
  name = "测试源",
  base_url = "` + baseURL + `",
  region = "测试地区",
}

function source.list(ctx)
  return ctx:get("/list.html"):each_checked("ul.list li", function(row)
    local a = row:find("a")
    local url = ctx:abs(a:attr("href"))
    if url == "" then return end
    return {
      external_id = url,
      title       = a:text(),
      url         = url,
      published   = row:find("span.time"):text(),
    }
  end)
end

function source.detail(ctx, item)
  return ctx:get(item.url):find("div.article"):body()
end

return source
`
}

func TestScriptCollectsItems(t *testing.T) {
	srv := serveList(t)
	src := loadOne(t, "test.lua", listScript(srv.URL))

	if src.Key() != "test_src" || src.Name() != "测试源" || src.BaseURL() != srv.URL {
		t.Fatalf("metadata: key=%q name=%q base=%q", src.Key(), src.Name(), src.BaseURL())
	}

	items, err := src.List(context.Background(), testFetcher(t))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2: %+v", len(items), items)
	}

	first := items[0]
	if first.Title != "成人高考报名通知" {
		t.Errorf("title = %q", first.Title)
	}
	// Relative hrefs resolve against base_url.
	if first.URL != srv.URL+"/detail/1.html" {
		t.Errorf("url = %q, want it resolved against the base", first.URL)
	}
	if first.ExternalID != first.URL {
		t.Errorf("external_id = %q, want the resolved url", first.ExternalID)
	}
	// The source-level default is applied to every row.
	if first.Region != "测试地区" {
		t.Errorf("region = %q, want the source default", first.Region)
	}
	// Lua dates go through the same parser as the Go collectors, including the
	// 09:00 local anchor.
	if first.PublishedAt == nil {
		t.Fatal("published_at is nil")
	}
	if h := first.PublishedAt.Hour(); h != 9 {
		t.Errorf("published hour = %d, want 9 to match the Go collectors", h)
	}
	if first.PublishedRaw != "2026-09-04" {
		t.Errorf("published_raw = %q, want the text the page printed", first.PublishedRaw)
	}
}

func TestScriptDetailReturnsBody(t *testing.T) {
	srv := serveList(t)
	src := loadOne(t, "test.lua", listScript(srv.URL))

	items, err := src.List(context.Background(), testFetcher(t))
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	detailed, err := src.Detail(context.Background(), testFetcher(t), items[0])
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}
	if !strings.Contains(detailed.Content, "第一段正文") {
		t.Errorf("content = %q", detailed.Content)
	}
	// Block boundaries must survive: a body collapsed onto one line is
	// unreadable on the detail page.
	if !strings.Contains(detailed.Content, "\n") {
		t.Errorf("content lost its paragraph breaks: %q", detailed.Content)
	}
}

func TestEachCheckedRaisesWhenSelectorStopsMatching(t *testing.T) {
	// Serving markup the selector does not match is exactly what a site
	// redesign looks like. The run must fail with the selector named, not come
	// back as an empty-but-successful crawl.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><body><div class="brand-new-layout">x</div></body></html>`))
	}))
	defer srv.Close()

	src := loadOne(t, "test.lua", listScript(srv.URL))
	_, err := src.List(context.Background(), testFetcher(t))
	if err == nil {
		t.Fatal("expected an error when the selector matches nothing")
	}
	if !strings.Contains(err.Error(), "ul.list li") {
		t.Errorf("error should name the selector, got: %v", err)
	}
}

func TestContextFailAborts(t *testing.T) {
	srv := serveList(t)
	src := loadOne(t, "test.lua", `
local source = { key = "test_src", name = "t", base_url = "`+srv.URL+`" }
function source.list(ctx)
  ctx:fail("自定义的失败信息")
end
return source
`)

	_, err := src.List(context.Background(), testFetcher(t))
	if err == nil || !strings.Contains(err.Error(), "自定义的失败信息") {
		t.Errorf("expected the script's own failure message, got: %v", err)
	}
}

func TestSandboxHidesFilesystemAndProcess(t *testing.T) {
	srv := serveList(t)
	src := loadOne(t, "test.lua", `
local source = { key = "test_src", name = "t", base_url = "`+srv.URL+`" }
function source.list(ctx)
  if os then ctx:fail("os is reachable") end
  if io then ctx:fail("io is reachable") end
  if require then ctx:fail("require is reachable") end
  if dofile then ctx:fail("dofile is reachable") end
  if loadfile then ctx:fail("loadfile is reachable") end
  if debug then ctx:fail("debug is reachable") end
  return {}
end
return source
`)

	if _, err := src.List(context.Background(), testFetcher(t)); err != nil {
		t.Errorf("sandbox leaked: %v", err)
	}
}

func TestCancelledContextAbortsRunawayScript(t *testing.T) {
	srv := serveList(t)
	src := loadOne(t, "test.lua", `
local source = { key = "test_src", name = "t", base_url = "`+srv.URL+`" }
function source.list(ctx)
  local n = 0
  while true do n = n + 1 end
end
return source
`)

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := src.List(ctx, testFetcher(t))
	if err == nil {
		t.Fatal("expected an infinite loop to be aborted by the context")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("aborting took %v; the context is not reaching the VM loop", elapsed)
	}
}

func TestRowsWithoutIdentityAreSkipped(t *testing.T) {
	srv := serveList(t)
	src := loadOne(t, "test.lua", `
local source = { key = "test_src", name = "t", base_url = "`+srv.URL+`" }
function source.list(ctx)
  return ctx:get("/list.html"):each("ul.list li", function(row)
    return { title = "有标题但没有链接" }
  end)
end
return source
`)

	// Every row is unusable, which is a script bug rather than an empty page,
	// and must be reported as such.
	_, err := src.List(context.Background(), testFetcher(t))
	if err == nil {
		t.Fatal("expected an error when no row has a usable identity")
	}
	if !strings.Contains(err.Error(), "stable identity") {
		t.Errorf("error should explain the missing identity, got: %v", err)
	}
}

func TestRowsFilteredByNilReturnAreNotCountedAsBroken(t *testing.T) {
	srv := serveList(t)
	src := loadOne(t, "test.lua", `
local source = { key = "test_src", name = "t", base_url = "`+srv.URL+`" }
function source.list(ctx)
  return ctx:get("/list.html"):each_checked("ul.list li", function(row)
    local a = row:find("a")
    local url = ctx:abs(a:attr("href"))
    if url == "" then return end   -- deliberate filter, not a broken row
    return { title = a:text(), url = url }
  end)
end
return source
`)

	items, err := src.List(context.Background(), testFetcher(t))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 2 {
		t.Errorf("got %d items, want 2", len(items))
	}
}

// ---------------------------------------------------------------------------
// Loading and validation

func TestMissingBaseURLIsRejected(t *testing.T) {
	dir := writeScript(t, "nobase.lua", `
return { key = "nobase", name = "无 base_url" }
`)
	res, err := LoadDir(dir, testLoc, testLogger())
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(res.Sources) != 0 {
		t.Fatal("a script without base_url must not load")
	}
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0].Error(), "base_url") {
		t.Fatalf("expected a base_url error, got: %v", res.Errors)
	}
}

func TestKeyCollidingWithBuiltinIsRejected(t *testing.T) {
	// "demo" is registered by the collector package's init. Registering over
	// it would make its scraped rows unreachable.
	dir := writeScript(t, "clash.lua", `
return { key = "demo", name = "冒名顶替", base_url = "https://example.com" }
`)
	res, err := LoadDir(dir, testLoc, testLogger())
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(res.Sources) != 0 {
		t.Fatal("a key collision must not load")
	}
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0].Error(), "already used") {
		t.Fatalf("expected a collision error, got: %v", res.Errors)
	}
}

func TestSyntaxErrorNamesTheFile(t *testing.T) {
	dir := writeScript(t, "broken.lua", `
local source = { key = "broken", base_url = "https://example.com"
-- missing closing brace and `+"`return source`"+`
`)
	res, err := LoadDir(dir, testLoc, testLogger())
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("expected one error, got: %v", res.Errors)
	}
	if !strings.Contains(res.Errors[0].Error(), "broken.lua") {
		t.Errorf("error should name the file, got: %v", res.Errors[0])
	}
}

func TestRuntimeErrorAtLoadNamesTheFile(t *testing.T) {
	dir := writeScript(t, "boom.lua", `
error("初始化就炸了")
`)
	res, err := LoadDir(dir, testLoc, testLogger())
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(res.Errors) != 1 || !strings.Contains(res.Errors[0].Error(), "boom.lua") {
		t.Fatalf("expected a load error naming the file, got: %v", res.Errors)
	}
}

func TestKeyFallsBackToFilename(t *testing.T) {
	dir := writeScript(t, "my_source.lua", `
return { name = "没有 key", base_url = "https://example.com" }
`)
	res, err := LoadDir(dir, testLoc, testLogger())
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(res.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", res.Errors)
	}
	if len(res.Sources) != 1 || res.Sources[0].Key() != "my_source" {
		t.Fatalf("expected the filename to become the key, got %v", res.Sources)
	}
}

func TestInvalidKeyIsRejected(t *testing.T) {
	dir := writeScript(t, "bad.lua", `
return { key = "有中文的 key", base_url = "https://example.com" }
`)
	res, err := LoadDir(dir, testLoc, testLogger())
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(res.Sources) != 0 || len(res.Errors) != 1 {
		t.Fatalf("expected the invalid key to be rejected, got sources=%v errors=%v",
			res.Sources, res.Errors)
	}
}

func TestMissingPluginsDirIsNotAnError(t *testing.T) {
	res, err := LoadDir(filepath.Join(t.TempDir(), "does-not-exist"), testLoc, testLogger())
	if err != nil {
		t.Fatalf("a missing plugins directory must not be an error, got: %v", err)
	}
	if len(res.Sources) != 0 {
		t.Errorf("expected no sources, got %d", len(res.Sources))
	}
}

func TestNonLuaFilesAreIgnored(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "README.md"), "# not a script")
	mustWrite(t, filepath.Join(dir, ".hidden.lua"), `error("should never run")`)
	mustWrite(t, filepath.Join(dir, "real.lua"), `
return { key = "real", name = "真脚本", base_url = "https://example.com" }
`)

	res, err := LoadDir(dir, testLoc, testLogger())
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(res.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", res.Errors)
	}
	if len(res.Sources) != 1 || res.Sources[0].Key() != "real" {
		t.Fatalf("expected only real.lua to load, got %v", res.Sources)
	}
}

func TestScriptIsNotGivenASharedState(t *testing.T) {
	// Each call gets a fresh interpreter, so state cannot leak between runs.
	srv := serveList(t)
	src := loadOne(t, "test.lua", `
local calls = 0
local source = { key = "test_src", name = "t", base_url = "`+srv.URL+`" }
function source.list(ctx)
  calls = calls + 1
  if calls > 1 then ctx:fail("state leaked between runs") end
  return {}
end
return source
`)

	f := testFetcher(t)
	if _, err := src.List(context.Background(), f); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if _, err := src.List(context.Background(), f); err != nil {
		t.Fatalf("second run saw leaked state: %v", err)
	}
}

func TestItemToTableExposesItemToDetail(t *testing.T) {
	srv := serveList(t)
	src := loadOne(t, "test.lua", `
local source = { key = "test_src", name = "t", base_url = "`+srv.URL+`" }
function source.list(ctx) return {} end
function source.detail(ctx, item)
  return "标题是：" .. item.title .. "，链接是：" .. item.url
end
return source
`)

	got, err := src.Detail(context.Background(), testFetcher(t), model.Item{
		Title: "原标题", URL: "https://example.com/a.html",
	})
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}
	if got.Content != "标题是：原标题，链接是：https://example.com/a.html" {
		t.Errorf("content = %q", got.Content)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
