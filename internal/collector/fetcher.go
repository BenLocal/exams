package collector

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html/charset"
)

const (
	// maxBodyBytes caps a single response. Chinese government sites sometimes
	// serve multi-megabyte pages; without a cap a redirect loop into a huge
	// file can exhaust memory.
	maxBodyBytes = 8 << 20 // 8 MiB

	maxAttempts = 3
)

// Fetcher performs the HTTP work shared by every collector: context-aware
// requests, a browser-like User-Agent, per-host pacing, retries and charset
// decoding.
type Fetcher struct {
	client *http.Client
	ua     string
	hosts  *hostPacer
	loc    *time.Location

	// When dumpDir is set every fetched page is also written there, so the
	// raw markup can be inspected offline while correcting selectors.
	dumpMu  sync.Mutex
	dumpDir string
	dumpSeq int
}

// SetDumpDir makes every subsequent fetch write its decoded body to dir.
func (f *Fetcher) SetDumpDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create dump dir %s: %w", dir, err)
	}
	f.dumpMu.Lock()
	f.dumpDir = dir
	f.dumpMu.Unlock()
	return nil
}

// dump writes one fetched page, best-effort. A failure here must never break a
// crawl.
func (f *Fetcher) dump(pageURL string, body []byte) {
	f.dumpMu.Lock()
	defer f.dumpMu.Unlock()
	if f.dumpDir == "" {
		return
	}
	f.dumpSeq++
	u, err := url.Parse(pageURL)
	name := "page"
	if err == nil {
		name = strings.NewReplacer("/", "_", "?", "_", "&", "_", ":", "").Replace(u.Host + u.Path)
		if name == "" {
			name = "page"
		}
	}
	path := filepath.Join(f.dumpDir, fmt.Sprintf("%02d-%s.html", f.dumpSeq, truncateName(name, 80)))
	_ = os.WriteFile(path, body, 0o644)
}

func truncateName(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Loc is the timezone dates from the sources are interpreted in. Announcement
// dates are Chinese local dates with no zone attached; parsing them as UTC
// would shift every one of them by eight hours.
func (f *Fetcher) Loc() *time.Location { return f.loc }

// NewFetcher builds a Fetcher. proxyURL may be empty to use the environment.
func NewFetcher(ua, proxyURL string, timeout time.Duration, loc *time.Location) (*Fetcher, error) {
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     60 * time.Second,
		// Government sites frequently have incomplete certificate chains.
		// InsecureSkipVerify stays false: failing loudly on a bad certificate
		// is better than silently trusting anything.
		TLSHandshakeTimeout: 15 * time.Second,
	}
	if proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("parse HTTP_PROXY %q: %w", proxyURL, err)
		}
		transport.Proxy = http.ProxyURL(u)
	}

	return &Fetcher{
		client: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return errors.New("stopped after 5 redirects")
				}
				return nil
			},
		},
		ua:    ua,
		hosts: newHostPacer(700 * time.Millisecond),
		loc:   loc,
	}, nil
}

// GetBytes fetches a URL and returns its body decoded to UTF-8.
func (f *Fetcher) GetBytes(ctx context.Context, rawURL string) ([]byte, error) {
	body, _, err := f.get(ctx, rawURL)
	return body, err
}

// Get fetches a URL and parses it as an HTML document.
func (f *Fetcher) Get(ctx context.Context, rawURL string) (*goquery.Document, error) {
	body, _, err := f.get(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", rawURL, err)
	}
	return doc, nil
}

// get performs one logical fetch, retrying transient failures, and returns the
// UTF-8 body plus the final URL after redirects.
func (f *Fetcher) get(ctx context.Context, rawURL string) ([]byte, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, "", fmt.Errorf("parse url %q: %w", rawURL, err)
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := f.hosts.wait(ctx, u.Host); err != nil {
			return nil, "", err
		}

		body, finalURL, retryAfter, err := f.attempt(ctx, rawURL)
		if err == nil {
			return body, finalURL, nil
		}
		lastErr = err

		// Only transient failures are worth retrying.
		if retryAfter < 0 {
			return nil, "", err
		}
		if attempt == maxAttempts {
			break
		}

		wait := retryAfter
		if wait <= 0 {
			wait = time.Duration(attempt) * time.Second
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	}
	return nil, "", fmt.Errorf("fetch %s: %w", rawURL, lastErr)
}

// attempt makes a single request.
//
// The returned retryAfter is negative when the error is permanent, and zero
// when it is transient with no server-specified delay.
func (f *Fetcher) attempt(ctx context.Context, rawURL string) (body []byte, finalURL string, retryAfter time.Duration, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", -1, err
	}
	req.Header.Set("User-Agent", f.ua)
	// Go's default User-Agent is "Go-http-client/1.1", which many Chinese
	// government sites reject outright.
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Referer", rawURL)

	// Accept-Encoding is deliberately NOT set here. Go's transport adds
	// "Accept-Encoding: gzip" itself and transparently decompresses the
	// response — but only when the transport is the one that added the header.
	// Setting it manually disables that decompression, so a gzip response
	// arrives as raw binary and every parse downstream fails with no useful
	// error. Leaving it unset also means brotli is never negotiated, which is
	// what we want since Go cannot decode it without another dependency.

	resp, err := f.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", -1, ctx.Err()
		}
		return nil, "", 0, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
	}()

	switch {
	case resp.StatusCode == http.StatusTooManyRequests ||
		resp.StatusCode == http.StatusServiceUnavailable ||
		resp.StatusCode == http.StatusBadGateway:
		return nil, "", parseRetryAfter(resp.Header.Get("Retry-After")),
			fmt.Errorf("http %s from %s", resp.Status, rawURL)
	case resp.StatusCode != http.StatusOK:
		return nil, "", -1, fmt.Errorf("http %s from %s", resp.Status, rawURL)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, "", 0, fmt.Errorf("read body: %w", err)
	}

	decoded, err := decodeToUTF8(raw, resp.Header.Get("Content-Type"))
	if err != nil {
		return nil, "", -1, fmt.Errorf("decode %s: %w", rawURL, err)
	}
	// Dumped after decoding, so the saved file is what the parser actually saw
	// rather than a GB2312 byte soup.
	f.dump(rawURL, decoded)
	return decoded, resp.Request.URL.String(), 0, nil
}

// decodeToUTF8 converts a response body to UTF-8.
//
// This must not be hand-rolled. A large share of Chinese government sites —
// especially provincial 人事考试网 — serve GB2312, and many of them lie in the
// Content-Type header while telling the truth only in a <meta> tag. The
// charset package checks both, in the order a browser does.
func decodeToUTF8(body []byte, contentType string) ([]byte, error) {
	_, name, _ := charset.DetermineEncoding(body, contentType)
	if name == "utf-8" {
		return body, nil
	}
	reader, err := charset.NewReader(bytes.NewReader(body), contentType)
	if err != nil {
		// Serve the bytes as-is rather than losing the page entirely.
		return body, nil
	}
	return io.ReadAll(reader)
}

// ResolveURL resolves a possibly-relative href against a base URL and strips
// the fragment and tracking parameters.
func ResolveURL(base, href string) (string, error) {
	href = strings.TrimSpace(href)
	if href == "" || strings.HasPrefix(href, "javascript:") ||
		strings.HasPrefix(href, "#") || strings.HasPrefix(href, "mailto:") {
		return "", fmt.Errorf("unusable href %q", href)
	}
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	ref, err := url.Parse(href)
	if err != nil {
		return "", err
	}
	abs := b.ResolveReference(ref)

	abs.Fragment = ""
	q := abs.Query()
	for _, key := range []string{"utm_source", "utm_medium", "utm_campaign", "utm_term", "utm_content", "spm"} {
		q.Del(key)
	}
	abs.RawQuery = q.Encode()
	return abs.String(), nil
}

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		if secs > 60 {
			secs = 60
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			if d > time.Minute {
				return time.Minute
			}
			return d
		}
	}
	return 0
}

// hostPacer enforces a minimum gap between requests to the same host, so a
// source with many pages is not hammered.
type hostPacer struct {
	mu   sync.Mutex
	gap  time.Duration
	last map[string]time.Time
}

func newHostPacer(gap time.Duration) *hostPacer {
	return &hostPacer{gap: gap, last: map[string]time.Time{}}
}

func (p *hostPacer) wait(ctx context.Context, host string) error {
	p.mu.Lock()
	next := p.last[host].Add(p.gap)
	now := time.Now()
	if !next.After(now) {
		p.last[host] = now
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()

	select {
	case <-time.After(next.Sub(now)):
	case <-ctx.Done():
		return ctx.Err()
	}

	p.mu.Lock()
	p.last[host] = time.Now()
	p.mu.Unlock()
	return nil
}
