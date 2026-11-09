package importer

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"mime/multipart"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// PyPI: the simple repository API (PEP 503 HTML or PEP 691 JSON) for the
// project list and files, with sha256 from the hashes (or URL fragments),
// requires-python, yanked and upload-time (PEP 700). Files are uploaded with
// the legacy upload API twine uses, with the core metadata read from the
// wheel's METADATA or the sdist's PKG-INFO. Yanked releases are yanked again.

func init() {
	register("pypi", func(j *job) formatAdapter { return &pypiAdapter{j: j} })
	warmers["pypi"] = warmPyPI
}

const pypiJSON = "application/vnd.pypi.simple.v1+json"

var pypiAccept = http.Header{"Accept": {pypiJSON + ", application/vnd.pypi.simple.v1+html;q=0.2, text/html;q=0.1"}}

type pypiFile struct {
	Filename       string `json:"filename"`
	URL            string `json:"url"`
	SHA256         string `json:"-"`
	RequiresPython string `json:"-"`
	Yanked         bool   `json:"-"`
	Uploaded       time.Time
}

type pypiAdapter struct{ j *job }

var anchorRE = regexp.MustCompile(`(?is)<a\s([^>]*)>(.*?)</a>`)
var attrRE = regexp.MustCompile(`(?is)([a-z-]+)\s*(?:=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+)))?`)

// parseSimple reads a simple index page (JSON or HTML) into files; base
// resolves relative links. For the project list, names are the anchors' texts.
func parseSimple(body []byte, ctype, base string) (names []string, files []pypiFile, err error) {
	if strings.Contains(ctype, "json") {
		var doc struct {
			Projects []struct {
				Name string `json:"name"`
			} `json:"projects"`
			Files []struct {
				Filename       string            `json:"filename"`
				URL            string            `json:"url"`
				Hashes         map[string]string `json:"hashes"`
				RequiresPython string            `json:"requires-python"`
				Yanked         any               `json:"yanked"`
				UploadTime     string            `json:"upload-time"`
			} `json:"files"`
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			return nil, nil, fmt.Errorf("simple index JSON: %w", err)
		}
		for _, p := range doc.Projects {
			names = append(names, p.Name)
		}
		for _, f := range doc.Files {
			pf := pypiFile{Filename: f.Filename, URL: resolveRef(base, f.URL), SHA256: strings.ToLower(f.Hashes["sha256"]),
				RequiresPython: f.RequiresPython}
			switch y := f.Yanked.(type) {
			case bool:
				pf.Yanked = y
			case string:
				pf.Yanked = true
			}
			pf.Uploaded, _ = time.Parse(time.RFC3339, f.UploadTime)
			files = append(files, pf)
		}
		return names, files, nil
	}
	for _, m := range anchorRE.FindAllSubmatch(body, -1) {
		attrs := map[string]string{}
		for _, a := range attrRE.FindAllStringSubmatch(string(m[1]), -1) {
			attrs[strings.ToLower(a[1])] = html.UnescapeString(a[2] + a[3] + a[4])
		}
		text := strings.TrimSpace(html.UnescapeString(string(m[2])))
		href := attrs["href"]
		if href == "" {
			continue
		}
		names = append(names, text)
		link, frag, _ := strings.Cut(href, "#")
		pf := pypiFile{Filename: text, URL: resolveRef(base, link), RequiresPython: attrs["data-requires-python"]}
		if _, ok := attrs["data-yanked"]; ok {
			pf.Yanked = true
		}
		if algo, sum, ok := strings.Cut(frag, "="); ok && algo == "sha256" {
			pf.SHA256 = strings.ToLower(sum)
		}
		files = append(files, pf)
	}
	return names, files, nil
}

func resolveRef(base, ref string) string {
	b, err := url.Parse(base)
	if err != nil {
		return ref
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}

// pypiFileVersion returns the version in a distribution file name and
// whether it is a wheel; ok is false for other files.
func pypiFileVersion(filename string) (version string, wheel, ok bool) {
	lower := strings.ToLower(filename)
	switch {
	case strings.HasSuffix(lower, ".whl"):
		p := strings.Split(strings.TrimSuffix(filename, ".whl"), "-")
		if len(p) < 5 {
			return "", false, false
		}
		return p[1], true, true
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".zip"), strings.HasSuffix(lower, ".tar.bz2"):
		stem := filename[:len(filename)-len(".tar.gz")]
		if strings.HasSuffix(lower, ".zip") {
			stem = filename[:len(filename)-len(".zip")]
		} else if strings.HasSuffix(lower, ".tar.bz2") {
			stem = filename[:len(filename)-len(".tar.bz2")]
		}
		i := strings.LastIndexByte(stem, '-')
		if i <= 0 {
			return "", false, false
		}
		return stem[i+1:], false, true
	}
	return "", false, false
}

func (a *pypiAdapter) page(ctx context.Context, ref string) ([]string, []pypiFile, error) {
	u, err := a.j.src.resolve(ref)
	if err != nil {
		return nil, nil, err
	}
	body, hdr, err := a.j.src.bytes(ctx, ref, pypiAccept)
	if err != nil {
		return nil, nil, err
	}
	return parseSimple(body, hdr.Get("Content-Type"), u.String())
}

func (a *pypiAdapter) list(ctx context.Context) ([]string, error) {
	names, _, err := a.page(ctx, "")
	return names, err
}

func (a *pypiAdapter) versions(ctx context.Context, name string) ([]srcVersion, error) {
	_, files, err := a.page(ctx, url.PathEscape(normalizePyPI(name))+"/")
	if err != nil {
		return nil, err
	}
	byVersion := map[string][]pypiFile{}
	var order []string
	for _, f := range files {
		v, _, ok := pypiFileVersion(f.Filename)
		if !ok {
			continue
		}
		if _, seen := byVersion[v]; !seen {
			order = append(order, v)
		}
		byVersion[v] = append(byVersion[v], f)
	}
	out := make([]srcVersion, 0, len(order))
	for _, v := range order {
		sv := srcVersion{Version: v, Data: byVersion[v]}
		for _, f := range byVersion[v] {
			if !f.Uploaded.IsZero() && (sv.Published.IsZero() || f.Uploaded.Before(sv.Published)) {
				sv.Published = f.Uploaded
			}
		}
		out = append(out, sv)
	}
	return out, nil
}

func (a *pypiAdapter) copy(ctx context.Context, name string, v srcVersion) (outcome, error) {
	files, _ := v.Data.([]pypiFile)
	project := normalizePyPI(name)
	have, err := a.j.existingAssets(ctx, "", project, v.Version)
	if err != nil {
		return 0, err
	}
	result := skipped
	yanked := len(files) > 0
	for _, f := range files {
		yanked = yanked && f.Yanked
		o, err := a.copyFile(ctx, project, v, f, have)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", f.Filename, err)
		}
		switch {
		case o == imported:
			result = imported
		case o == conflict && result == skipped:
			result = conflict
		}
	}
	if yanked && result == imported {
		r, err := a.j.put(ctx, http.MethodPost, "yank/"+url.PathEscape(project)+"/"+url.PathEscape(v.Version), nil, nil, 0, time.Time{})
		if err == nil && !r.ok() {
			a.j.log.Warnf("%s %s: imported, but yanking it failed: %s", name, v.Version, r.message())
		}
	}
	return result, nil
}

func (a *pypiAdapter) copyFile(ctx context.Context, project string, v srcVersion, f pypiFile, have []*core.Asset) (outcome, error) {
	if have != nil && f.SHA256 != "" {
		if hasContent(have, sums{"sha256": f.SHA256}) {
			return skipped, nil
		}
		if slices.ContainsFunc(have, func(x *core.Asset) bool { return strings.HasSuffix(x.Path, "/"+f.Filename) }) {
			return conflict, nil
		}
	}
	dl, err := a.j.fetch(ctx, f.URL, nil)
	if err != nil {
		return 0, err
	}
	defer dl.Close()
	if f.SHA256 != "" && dl.sha256 != f.SHA256 {
		return 0, fmt.Errorf("sha256 %s, the index says %s", dl.sha256, f.SHA256)
	}
	if have != nil && hasContent(have, sums{"sha256": dl.sha256}) {
		return skipped, nil
	}
	_, wheel, _ := pypiFileVersion(f.Filename)
	meta, err := distMetadata(dl, f.Filename, wheel)
	if err != nil {
		return 0, err
	}
	fields := [][2]string{{":action", "file_upload"}, {"protocol_version", "1"}, {"name", project}, {"version", v.Version},
		{"sha256_digest", dl.sha256}, {"md5_digest", dl.md5}}
	if wheel {
		p := strings.Split(strings.TrimSuffix(f.Filename, ".whl"), "-")
		fields = append(fields, [2]string{"filetype", "bdist_wheel"}, [2]string{"pyversion", p[len(p)-3]})
	} else {
		fields = append(fields, [2]string{"filetype", "sdist"}, [2]string{"pyversion", "source"})
	}
	hasRP := false
	for _, kv := range meta {
		switch kv[0] {
		case "name", "version":
			if kv[0] == "name" {
				fields[2][1] = kv[1] // display name as the project spells it
			}
			continue
		case "requires_python":
			hasRP = true
		}
		fields = append(fields, kv)
	}
	if !hasRP && f.RequiresPython != "" {
		fields = append(fields, [2]string{"requires_python", f.RequiresPython})
	}
	body, err := dl.reader()
	if err != nil {
		return 0, err
	}
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		var err error
		for _, kv := range fields {
			if err = mw.WriteField(kv[0], kv[1]); err != nil {
				break
			}
		}
		if err == nil {
			var part io.Writer
			if part, err = mw.CreateFormFile("content", f.Filename); err == nil {
				_, err = io.Copy(part, body)
			}
		}
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()
	resp, err := a.j.put(ctx, http.MethodPost, "legacy/", http.Header{"Content-Type": {mw.FormDataContentType()}}, pr, -1, v.Published)
	_ = pr.Close()
	if err != nil {
		return 0, err
	}
	return uploadOutcome(resp, f.Filename)
}

// distMetadata reads the core metadata of a wheel (METADATA) or an sdist
// (PKG-INFO) as upload form fields.
func distMetadata(dl *download, filename string, wheel bool) ([][2]string, error) {
	var raw []byte
	lower := strings.ToLower(filename)
	switch {
	case wheel || strings.HasSuffix(lower, ".zip"):
		zr, err := zip.NewReader(dl.f, dl.size)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", filename, err)
		}
		for _, f := range zr.File {
			parts := strings.Split(f.Name, "/")
			if (wheel && len(parts) == 2 && strings.HasSuffix(parts[0], ".dist-info") && parts[1] == "METADATA") ||
				(!wheel && len(parts) == 2 && parts[1] == "PKG-INFO") {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				raw, err = io.ReadAll(io.LimitReader(rc, 4<<20))
				_ = rc.Close()
				if err != nil {
					return nil, err
				}
				break
			}
		}
	case strings.HasSuffix(lower, ".tar.gz"):
		r, err := dl.reader()
		if err != nil {
			return nil, err
		}
		gz, err := gzip.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", filename, err)
		}
		tr := tar.NewReader(gz)
		for {
			h, err := tr.Next()
			if err != nil {
				break
			}
			if parts := strings.Split(strings.TrimPrefix(h.Name, "./"), "/"); len(parts) == 2 && parts[1] == "PKG-INFO" {
				raw, _ = io.ReadAll(io.LimitReader(tr, 4<<20))
				break
			}
		}
	}
	if raw == nil {
		return nil, nil // the upload works without; summary and classifiers stay empty
	}
	msg, err := mail.ReadMessage(bufio.NewReader(strings.NewReader(string(raw) + "\n")))
	if err != nil {
		return nil, nil //nolint:nilerr // unreadable metadata: upload without it
	}
	var out [][2]string
	keys := make([]string, 0, len(msg.Header))
	for k := range msg.Header {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		field := strings.ReplaceAll(strings.ToLower(k), "-", "_")
		for _, v := range msg.Header[k] {
			out = append(out, [2]string{field, v})
		}
	}
	if desc, err := io.ReadAll(io.LimitReader(msg.Body, 1<<20)); err == nil && strings.TrimSpace(string(desc)) != "" {
		out = append(out, [2]string{"description", strings.TrimSpace(string(desc))})
	}
	return out, nil
}

func (a *pypiAdapter) coords(name string, v srcVersion) []string {
	return []string{normalizePyPI(name) + "==" + v.Version}
}

// warmPyPI fetches the project page pip reads, then every file of the version.
func warmPyPI(ctx context.Context, w *warmJob, c coord) error {
	rel := "simple/" + url.PathEscape(c.name) + "/"
	r, err := w.get(ctx, rel, pypiAccept, maxDocBytes)
	if err != nil {
		return err
	}
	base := repoPath(w.repo.Name, rel)
	_, files, err := parseSimple(r.Body, r.Header.Get("Content-Type"), base)
	if err != nil {
		return err
	}
	n := 0
	for _, f := range files {
		v, _, ok := pypiFileVersion(f.Filename)
		if !ok || compareVersions(v, c.version) != 0 {
			continue
		}
		if err := w.fetchLink(ctx, base, f.URL); err != nil {
			return fmt.Errorf("%s: %w", f.Filename, err)
		}
		n++
	}
	if n == 0 {
		return fmt.Errorf("version %s has no files: %w", c.version, errNotFound)
	}
	return nil
}
