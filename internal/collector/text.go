package collector

import (
	"strings"

	"github.com/BenLocal/exams/internal/model"
	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"
)

// blockTags are elements whose boundaries become paragraph breaks in the
// extracted text.
var blockTags = map[string]bool{
	"p": true, "div": true, "li": true, "tr": true, "td": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"section": true, "article": true, "blockquote": true, "pre": true,
	"ul": true, "ol": true, "table": true, "tbody": true, "thead": true,
	"figure": true, "figcaption": true, "hr": true,
}

// TextWithBreaks extracts a node's text with paragraph breaks preserved.
//
// goquery's Text() concatenates text nodes and inserts nothing between block
// elements, so a page of <p> paragraphs comes out as one unbroken line. A
// reader-facing body needs the boundaries put back.
//
// The result is passed through model.NormalizeBody, which collapses the
// incidental whitespace of the markup while keeping the paragraph breaks
// introduced here.
func TextWithBreaks(sel *goquery.Selection) string {
	var b strings.Builder
	for _, node := range sel.Nodes {
		writeText(&b, node)
	}
	return model.NormalizeBody(b.String())
}

func writeText(b *strings.Builder, n *html.Node) {
	switch n.Type {
	case html.TextNode:
		b.WriteString(n.Data)
		return
	case html.DocumentNode:
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			writeText(b, c)
		}
		return
	case html.ElementNode:
		// Fall through to the element handling below.
	default:
		return
	}

	name := strings.ToLower(n.Data)

	// Markup that is not prose. Crawled pages are full of inline scripts and
	// stylesheets, and none of it belongs in a stored body.
	if name == "script" || name == "style" || name == "noscript" || name == "head" {
		return
	}
	// A <br> is a single line break, not a paragraph.
	if name == "br" {
		b.WriteString("\n")
		return
	}

	block := blockTags[name]
	if block {
		b.WriteString("\n")
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		writeText(b, c)
	}
	if block {
		b.WriteString("\n")
	}
}
