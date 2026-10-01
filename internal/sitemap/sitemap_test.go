package sitemap

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const urlset = `<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9"
        xmlns:image="http://www.google.com/schemas/sitemap-image/1.1">
  <url>
    <loc>https://example.com/a.html</loc>
    <lastmod>2026-09-01</lastmod>
    <image:image><image:loc>https://cdn.example.com/a.jpg</image:loc></image:image>
  </url>
  <url><loc> https://example.com/b.htm </loc></url>
</urlset>`

func TestParseURLSetIgnoresImageLoc(t *testing.T) {
	d, err := ParseBytes([]byte(urlset))
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != KindURLSet || len(d.Entries) != 2 {
		t.Fatalf("doc = %+v", d)
	}
	if d.Entries[0].Loc != "https://example.com/a.html" || d.Entries[0].LastMod != "2026-09-01" {
		t.Errorf("entry0 = %+v", d.Entries[0])
	}
	if d.Entries[1].Loc != "https://example.com/b.htm" {
		t.Errorf("entry1 = %+v", d.Entries[1])
	}
}

func TestParseRejectsUnknownRoot(t *testing.T) {
	if _, err := ParseBytes([]byte(`<html><body/></html>`)); err == nil {
		t.Fatal("expected error")
	}
	if _, err := ParseBytes([]byte(``)); err == nil {
		t.Fatal("expected error on empty doc")
	}
}

func gz(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestWalkIndexWithGzipAndErrors(t *testing.T) {
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/index.xml.gz", func(w http.ResponseWriter, r *http.Request) {
		body := `<sitemapindex xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <sitemap><loc>` + srv.URL + `/s1.xml</loc></sitemap>
  <sitemap><loc>` + srv.URL + `/s2.xml.gz</loc></sitemap>
  <sitemap><loc>` + srv.URL + `/missing.xml</loc></sitemap>
</sitemapindex>`
		_, _ = w.Write(gz(t, body))
	})
	mux.HandleFunc("/s1.xml", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(urlset))
	})
	mux.HandleFunc("/s2.xml.gz", func(w http.ResponseWriter, r *http.Request) {
		body := `<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url><loc>https://example.com/a.html</loc></url>
  <url><loc>https://example.com/c.html</loc></url>
</urlset>`
		_, _ = w.Write(gz(t, body))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient("gscx-test")
	res, err := c.Walk(context.Background(), srv.URL+"/index.xml.gz", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Sitemaps) != 4 {
		t.Fatalf("sitemaps = %+v", res.Sitemaps)
	}
	if res.Sitemaps[0].Kind != KindIndex || res.Sitemaps[0].Entries != 3 {
		t.Errorf("root = %+v", res.Sitemaps[0])
	}
	var missing *SitemapResult
	for i := range res.Sitemaps {
		if strings.HasSuffix(res.Sitemaps[i].URL, "/missing.xml") {
			missing = &res.Sitemaps[i]
		}
	}
	if missing == nil || missing.Error == nil || missing.Error.Kind != "http_status" || missing.Error.StatusCode != 404 {
		t.Errorf("missing = %+v", missing)
	}
	// a.html appears in both children: deduplicated.
	if len(res.URLs) != 3 {
		t.Fatalf("urls = %+v", res.URLs)
	}
}

func TestFetchDNSError(t *testing.T) {
	c := NewClient("gscx-test")
	_, err := c.Fetch(context.Background(), "https://nonexistent.invalid/sitemap.xml")
	var fe *FetchError
	if !errors.As(err, &fe) || fe.Kind != "dns" {
		t.Fatalf("err = %v", err)
	}
}

func TestCapReaderRejectsOversize(t *testing.T) {
	r := &capReader{r: strings.NewReader("abcdef"), left: 3}
	buf := make([]byte, 10)
	n, err := r.Read(buf)
	if n != 3 || err != nil {
		t.Fatalf("first read n=%d err=%v", n, err)
	}
	if _, err := r.Read(buf); !errors.Is(err, errTooLarge) {
		t.Fatalf("expected errTooLarge, got %v", err)
	}
	r = &capReader{r: strings.NewReader("abc"), left: 3}
	if n, _ := r.Read(buf); n != 3 {
		t.Fatalf("exact fit n=%d", n)
	}
	if _, err := r.Read(buf); err == nil || errors.Is(err, errTooLarge) {
		t.Fatalf("exact fit should EOF, got %v", err)
	}
}
