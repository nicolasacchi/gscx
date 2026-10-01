// Package sitemap downloads and parses XML sitemaps (plain or gzipped,
// urlset or sitemapindex) and walks sitemap indexes.
package sitemap

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Kind is the root element of a sitemap document.
type Kind string

const (
	KindURLSet Kind = "urlset"
	KindIndex  Kind = "sitemapindex"
)

// MaxBytes bounds a single decompressed sitemap (the protocol limit is 50 MB).
const MaxBytes = 64 << 20

// Entry is one <url> or <sitemap> child.
type Entry struct {
	Loc     string `json:"loc"`
	LastMod string `json:"lastmod,omitempty"`
}

// Doc is a parsed sitemap.
type Doc struct {
	Kind    Kind    `json:"kind"`
	Entries []Entry `json:"entries"`
}

// FetchError describes why a sitemap could not be fetched or parsed.
type FetchError struct {
	URL string `json:"url"`
	// Kind is one of: dns, timeout, network, http_status, too_large, parse.
	Kind       string `json:"kind"`
	StatusCode int    `json:"status_code,omitempty"`
	Err        string `json:"error"`
}

func (e *FetchError) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("%s: %s (HTTP %d)", e.URL, e.Kind, e.StatusCode)
	}
	return fmt.Sprintf("%s: %s: %s", e.URL, e.Kind, e.Err)
}

// Client fetches sitemaps.
type Client struct {
	HTTP      *http.Client
	UserAgent string
}

// NewClient returns a Client with a sane timeout.
func NewClient(userAgent string) *Client {
	return &Client{HTTP: &http.Client{Timeout: 90 * time.Second}, UserAgent: userAgent}
}

// Fetch downloads and parses one sitemap. Gzip is detected by magic bytes, so
// both .xml.gz files and transport-compressed responses work.
func (c *Client) Fetch(ctx context.Context, url string) (*Doc, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, &FetchError{URL: url, Kind: "network", Err: err.Error()}
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, classifyNetErr(url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, &FetchError{URL: url, Kind: "http_status", StatusCode: resp.StatusCode, Err: resp.Status}
	}
	body, err := decompress(resp.Body)
	if err != nil {
		return nil, &FetchError{URL: url, Kind: "parse", Err: err.Error()}
	}
	doc, err := Parse(body)
	if err != nil {
		var fe *FetchError
		if errors.As(err, &fe) {
			fe.URL = url
			return nil, fe
		}
		return nil, &FetchError{URL: url, Kind: "parse", Err: err.Error()}
	}
	return doc, nil
}

func decompress(r io.Reader) (io.Reader, error) {
	br := bufio.NewReader(r)
	magic, err := br.Peek(2)
	if err == nil && magic[0] == 0x1f && magic[1] == 0x8b {
		zr, err := gzip.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("gzip: %w", err)
		}
		return limited(zr), nil
	}
	return limited(br), nil
}

// limited returns a reader that fails (instead of silently truncating) past
// MaxBytes.
func limited(r io.Reader) io.Reader {
	return &capReader{r: r, left: MaxBytes}
}

type capReader struct {
	r    io.Reader
	left int64
}

var errTooLarge = errors.New("sitemap exceeds size limit")

func (c *capReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		// Probe for more data: EOF here means the document fit exactly.
		var one [1]byte
		n, err := c.r.Read(one[:])
		if n > 0 {
			return 0, errTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}

// Parse decodes a sitemap document. Only <loc> elements that are direct
// children of <url> or <sitemap> are collected, so image/video extension
// locations (<image:loc>) are ignored.
func Parse(r io.Reader) (*Doc, error) {
	dec := xml.NewDecoder(r)
	doc := &Doc{}
	var stack []string
	var cur *Entry
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			if errors.Is(err, errTooLarge) {
				return nil, &FetchError{Kind: "too_large", Err: err.Error()}
			}
			return nil, fmt.Errorf("xml: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			name := t.Name.Local
			if len(stack) == 0 {
				switch name {
				case string(KindURLSet):
					doc.Kind = KindURLSet
				case string(KindIndex):
					doc.Kind = KindIndex
				default:
					return nil, fmt.Errorf("unexpected root element <%s>", name)
				}
			}
			if len(stack) == 1 && (name == "url" || name == "sitemap") {
				cur = &Entry{}
			}
			stack = append(stack, name)
		case xml.EndElement:
			if len(stack) == 2 && cur != nil {
				if cur.Loc != "" {
					doc.Entries = append(doc.Entries, *cur)
				}
				cur = nil
			}
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case xml.CharData:
			if cur == nil || len(stack) != 3 {
				continue
			}
			switch stack[2] {
			case "loc":
				cur.Loc += strings.TrimSpace(string(t))
			case "lastmod":
				cur.LastMod += strings.TrimSpace(string(t))
			}
		}
	}
	if doc.Kind == "" {
		return nil, errors.New("empty document")
	}
	return doc, nil
}

// SitemapResult is one sitemap visited by Walk.
type SitemapResult struct {
	URL     string      `json:"url"`
	Parent  string      `json:"parent,omitempty"`
	Kind    Kind        `json:"kind,omitempty"`
	Entries int         `json:"entries"`
	Error   *FetchError `json:"error,omitempty"`
}

// URL is a page URL found in a urlset, with the sitemap that listed it.
type URL struct {
	Loc     string `json:"loc"`
	LastMod string `json:"lastmod,omitempty"`
	Sitemap string `json:"sitemap"`
}

// WalkResult aggregates a walk.
type WalkResult struct {
	Sitemaps []SitemapResult `json:"sitemaps"`
	URLs     []URL           `json:"-"`
}

// Walk fetches root and, for sitemap indexes, every child (up to maxDepth
// levels, default 3), returning all page URLs (deduplicated by loc, first
// occurrence wins). Child fetch errors are recorded, not fatal; only a
// cancelled context aborts the walk.
func (c *Client) Walk(ctx context.Context, root string, maxDepth int) (*WalkResult, error) {
	if maxDepth <= 0 {
		maxDepth = 3
	}
	res := &WalkResult{}
	seenURL := map[string]bool{}
	seenMap := map[string]bool{root: true}

	type job struct{ url, parent string }
	level := []job{{url: root}}
	for depth := 0; depth < maxDepth && len(level) > 0; depth++ {
		docs := make([]*Doc, len(level))
		errs := make([]error, len(level))
		var wg sync.WaitGroup
		sem := make(chan struct{}, 4)
		for i, j := range level {
			wg.Add(1)
			go func(i int, u string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				docs[i], errs[i] = c.Fetch(ctx, u)
			}(i, j.url)
		}
		wg.Wait()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var next []job
		for i, j := range level {
			sr := SitemapResult{URL: j.url, Parent: j.parent}
			if errs[i] != nil {
				var fe *FetchError
				if !errors.As(errs[i], &fe) {
					fe = &FetchError{URL: j.url, Kind: "network", Err: errs[i].Error()}
				}
				sr.Error = fe
				res.Sitemaps = append(res.Sitemaps, sr)
				continue
			}
			d := docs[i]
			sr.Kind = d.Kind
			sr.Entries = len(d.Entries)
			res.Sitemaps = append(res.Sitemaps, sr)
			for _, e := range d.Entries {
				if d.Kind == KindIndex {
					if !seenMap[e.Loc] {
						seenMap[e.Loc] = true
						next = append(next, job{url: e.Loc, parent: j.url})
					}
					continue
				}
				if !seenURL[e.Loc] {
					seenURL[e.Loc] = true
					res.URLs = append(res.URLs, URL{Loc: e.Loc, LastMod: e.LastMod, Sitemap: j.url})
				}
			}
		}
		level = next
	}
	return res, nil
}

func classifyNetErr(url string, err error) *FetchError {
	fe := &FetchError{URL: url, Kind: "network", Err: err.Error()}
	var dnsErr *net.DNSError
	var netErr net.Error
	switch {
	case errors.As(err, &dnsErr):
		fe.Kind = "dns"
	case errors.As(err, &netErr) && netErr.Timeout():
		fe.Kind = "timeout"
	case errors.Is(err, context.DeadlineExceeded):
		fe.Kind = "timeout"
	}
	return fe
}

// ParseBytes is a convenience wrapper for tests and fixtures.
func ParseBytes(b []byte) (*Doc, error) { return Parse(bytes.NewReader(b)) }
