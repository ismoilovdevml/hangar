package raw

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/ismoilovdevml/hangar/internal/core"
)

func (fx *fixture) listJSON(path string) (int, listing) {
	fx.t.Helper()
	code, hdr, body := fx.do(http.MethodGet, path, nil, as("bob"), header("Accept", "application/json"))
	var l listing
	if code == http.StatusOK {
		if ct := hdr.Get("Content-Type"); ct != "application/json" {
			fx.t.Fatalf("listing Content-Type = %q", ct)
		}
		if err := json.Unmarshal([]byte(body), &l); err != nil {
			fx.t.Fatalf("listing JSON: %v: %s", err, body)
		}
	}
	return code, l
}

func names(l listing) []string {
	out := make([]string, 0, len(l.Entries))
	for _, e := range l.Entries {
		n := e.Name
		if e.Type == "directory" {
			n += "/"
		}
		out = append(out, n)
	}
	return out
}

func TestDirectoryListing(t *testing.T) {
	fx := newFixture(t)
	for _, p := range []string{
		"top.txt", "docs/a.txt", "docs/b.txt", "docs/sub/deep/c.txt", "docs/sub/d.txt",
		"docs/a-b.txt", "docs/a.b/x", "docs/a0", "docs/a/inner",
	} {
		if code := fx.put("raw-hosted/"+p, "content of "+p); code != http.StatusCreated {
			t.Fatalf("PUT %s = %d", p, code)
		}
	}

	code, root := fx.listJSON("raw-hosted/")
	if code != http.StatusOK {
		t.Fatalf("root listing = %d", code)
	}
	if got, want := names(root), []string{"docs/", "top.txt"}; !slices.Equal(got, want) {
		t.Errorf("root = %v, want %v", got, want)
	}
	if root.Path != "" || root.Repository != "raw-hosted" {
		t.Errorf("root path = %q repo = %q", root.Path, root.Repository)
	}

	_, docs := fx.listJSON("raw-hosted/docs/")
	// Byte order of names, directories compared with their trailing "/".
	want := []string{"a-b.txt", "a.b/", "a.txt", "a/", "a0", "b.txt", "sub/"}
	if got := names(docs); !slices.Equal(got, want) {
		t.Errorf("docs = %v, want %v", got, want)
	}
	for _, e := range docs.Entries {
		switch e.Type {
		case "file":
			if e.Size == 0 || e.Checksums["sha256"] == "" || e.LastModified == "" || !strings.HasPrefix(e.Path, "docs/") {
				t.Errorf("file entry incomplete: %+v", e)
			}
		case "directory":
			if !strings.HasSuffix(e.Path, "/") || e.Size != 0 {
				t.Errorf("dir entry: %+v", e)
			}
		}
	}

	// Pagination: walking any page size yields the same sorted listing.
	for _, limit := range []int{1, 2, 3, 7, 100} {
		var all []string
		cursor := ""
		for pages := 0; ; pages++ {
			if pages > 20 {
				t.Fatalf("limit %d: too many pages", limit)
			}
			q := url.Values{"limit": {fmt.Sprint(limit)}}
			if cursor != "" {
				q.Set("cursor", cursor)
			}
			code, l := fx.listJSON("raw-hosted/docs/?" + q.Encode())
			if code != http.StatusOK || len(l.Entries) > limit {
				t.Fatalf("limit %d page %d: %d, %d entries", limit, pages, code, len(l.Entries))
			}
			all = append(all, names(l)...)
			if l.NextCursor == "" {
				break
			}
			cursor = l.NextCursor
		}
		if !slices.Equal(all, want) {
			t.Errorf("limit %d: paged = %v, want %v", limit, all, want)
		}
	}

	if code, _ := fx.listJSON("raw-hosted/nope/"); code != http.StatusNotFound {
		t.Errorf("missing dir = %d", code)
	}
	if code, _ := fx.listJSON("raw-hosted/top.txt/"); code != http.StatusNotFound {
		t.Errorf("file as dir = %d", code)
	}
	if code, _, _ := fx.do(http.MethodGet, "raw-hosted/docs/?cursor=!!not-a-cursor", nil, as("bob")); code != http.StatusBadRequest {
		t.Errorf("bad cursor = %d", code)
	}
	if code, _, _ := fx.do(http.MethodGet, "raw-hosted/docs/?limit=0", nil, as("bob")); code != http.StatusBadRequest {
		t.Errorf("bad limit = %d", code)
	}

	// A directory path without its slash redirects to the listing.
	code, hdr, _ := fx.do(http.MethodGet, "raw-hosted/docs/sub", nil, as("bob"))
	if code != http.StatusFound || hdr.Get("Location") != "sub/" {
		t.Errorf("dir without slash = %d Location %q", code, hdr.Get("Location"))
	}
}

func TestEmptyRootListing(t *testing.T) {
	fx := newFixture(t)
	code, l := fx.listJSON("raw-hosted/")
	if code != http.StatusOK || len(l.Entries) != 0 || l.Entries == nil {
		t.Errorf("empty root = %d %+v", code, l)
	}
}

func TestHTMLListing(t *testing.T) {
	fx := newFixture(t)
	fx.put("raw-hosted/web/<img src=x onerror=alert(1)>.txt", "x")
	fx.put("raw-hosted/web/javascript:alert(1)", "x")
	fx.put("raw-hosted/web/sub dir/f", "x")
	for _, accept := range []string{"", "*/*", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"} {
		opts := []reqOpt{as("bob")}
		if accept != "" {
			opts = append(opts, header("Accept", accept))
		}
		code, hdr, body := fx.do(http.MethodGet, "raw-hosted/web/", nil, opts...)
		if code != http.StatusOK || !strings.HasPrefix(hdr.Get("Content-Type"), "text/html") {
			t.Fatalf("Accept %q: %d %s", accept, code, hdr.Get("Content-Type"))
		}
		if strings.Contains(body, "<img") {
			t.Errorf("unescaped name in HTML: %s", body)
		}
		for _, want := range []string{
			`href="../"`,
			`href="./%3Cimg%20src=x%20onerror=alert%281%29%3E.txt"`,
			`href="./javascript:alert%281%29"`,
			`href="./sub%20dir/"`,
			"&lt;img src=x onerror=alert(1)&gt;.txt",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("HTML missing %s:\n%s", want, body)
			}
		}
		if !strings.Contains(hdr.Get("Content-Security-Policy"), "default-src 'none'") {
			t.Errorf("CSP = %q", hdr.Get("Content-Security-Policy"))
		}
	}
	// Next-page link.
	_, _, body := fx.do(http.MethodGet, "raw-hosted/web/?limit=1", nil, as("bob"))
	if !strings.Contains(body, `href="?cursor=`) || !strings.Contains(body, "limit=1") {
		t.Errorf("no next link:\n%s", body)
	}
}

func TestWantsJSON(t *testing.T) {
	tests := []struct {
		accept []string
		want   bool
	}{
		{nil, false},
		{[]string{"*/*"}, false},
		{[]string{"application/json"}, true},
		{[]string{"application/json, text/plain, */*"}, true},
		{[]string{"text/html, application/json"}, true},
		{[]string{"text/html;q=1, application/json;q=0.5"}, false},
		{[]string{"application/json;q=0"}, false},
		{[]string{"text/html", "application/json;q=0.9"}, false},
	}
	for _, tc := range tests {
		if got := wantsJSON(tc.accept); got != tc.want {
			t.Errorf("wantsJSON(%q) = %v", tc.accept, got)
		}
	}
}

func TestGroupListingMerges(t *testing.T) {
	fx := newFixture(t,
		&core.Repository{Name: "raw-hosted2", Format: Name, Type: core.RepoHosted},
		&core.Repository{Name: "raw-group", Format: Name, Type: core.RepoGroup,
			Config: core.RepoConfig{Group: &core.GroupConfig{Members: []string{"raw-hosted", "raw-hosted2"}}}})
	for _, p := range []string{"d/a", "d/c", "d/shared", "d/x/1"} {
		fx.put("raw-hosted/"+p, "first:"+p)
	}
	for _, p := range []string{"d/b", "d/shared", "d/x/2", "d/y/1", "d/z"} {
		fx.put("raw-hosted2/"+p, "second:"+p)
	}
	want := []string{"a", "b", "c", "shared", "x/", "y/", "z"}
	_, l := fx.listJSON("raw-group/d/")
	if got := names(l); !slices.Equal(got, want) {
		t.Errorf("group listing = %v, want %v", got, want)
	}
	for _, e := range l.Entries {
		if e.Name == "shared" && e.Size != int64(len("first:d/shared")) {
			t.Errorf("shared entry should come from the first member: %+v", e)
		}
	}
	for limit := 1; limit <= 4; limit++ {
		var all []string
		cursor := ""
		for range 20 {
			q := url.Values{"limit": {fmt.Sprint(limit)}}
			if cursor != "" {
				q.Set("cursor", cursor)
			}
			_, l := fx.listJSON("raw-group/d/?" + q.Encode())
			all = append(all, names(l)...)
			if cursor = l.NextCursor; cursor == "" {
				break
			}
		}
		if !slices.Equal(all, want) {
			t.Errorf("group limit %d: %v", limit, all)
		}
	}
}

// TestListingLargeDirectory exercises store pagination (more assets than one
// store page) below a collapsed directory.
func TestListingLargeDirectory(t *testing.T) {
	fx := newFixture(t)
	repo := mustRepo(t, fx, "raw-hosted")
	ctx := asAlice()
	d, _, err := fx.env.Blob.Put(ctx, strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	var assets []*core.Asset
	for i := range listBatch + 10 {
		assets = append(assets, &core.Asset{Path: fmt.Sprintf("big/a/%05d", i), BlobDigest: d, Size: 1})
	}
	assets = append(assets, &core.Asset{Path: "big/b", BlobDigest: d, Size: 1}, &core.Asset{Path: "big/c/1", BlobDigest: d, Size: 1})
	if _, _, err := fx.env.Components.Publish(ctx, repo, core.PublishRequest{Assets: assets}); err != nil {
		t.Fatal(err)
	}
	for limit := 1; limit <= 3; limit++ {
		var all []string
		cursor := ""
		for range 10 {
			q := url.Values{"limit": {fmt.Sprint(limit)}}
			if cursor != "" {
				q.Set("cursor", cursor)
			}
			_, l := fx.listJSON("raw-hosted/big/?" + q.Encode())
			all = append(all, names(l)...)
			if cursor = l.NextCursor; cursor == "" {
				break
			}
		}
		if want := []string{"a/", "b", "c/"}; !slices.Equal(all, want) {
			t.Errorf("limit %d: %v, want %v", limit, all, want)
		}
	}
}
