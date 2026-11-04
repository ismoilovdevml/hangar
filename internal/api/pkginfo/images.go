package pkginfo

import (
	"context"
	"encoding/hex"
	"io"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// maxImages bounds the images resolved for one README.
const maxImages = 200

// Image locates a README image or the package icon: a file inside the
// package archive (Member) or a remote http(s) URL. The API turns it into
// a URL the browser may load (see the readmeimg package).
type Image struct {
	URL    string `json:"url,omitempty"`
	Member string `json:"member,omitempty"`
}

// sourceRepo is the version-control repository a package names, used to
// resolve README images that point into it.
type sourceRepo struct {
	url, commit, dir string
}

// ---------------------------------------------------------------- references

var (
	htmlComment = regexp.MustCompile(`<!--[\s\S]*?-->`)
	imgTag      = regexp.MustCompile(`(?i)<img\b[^>]*>`)
	refDef      = regexp.MustCompile(`(?m)^ {0,3}\[([^\]]+)\]:[ \t]*<?(\S+?)>?(?:[ \t]+["'(](.*)["')])?[ \t]*$`)
	entity      = regexp.MustCompile(`&(#[0-9]{1,7}|#[xX][0-9a-fA-F]{1,6}|amp|lt|gt|quot|apos);`)
)

// srcAttr matches the src attribute of an HTML tag.
var srcAttr = regexp.MustCompile(`(?i)\ssrc\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+))`)

// imgSrc returns the src attribute of an <img> tag, entities decoded.
func imgSrc(tag string) string {
	m := srcAttr.FindStringSubmatch(tag)
	if m == nil {
		return ""
	}
	for _, v := range m[1:] {
		if v != "" {
			return decodeEntities(v)
		}
	}
	return ""
}

// decodeEntities decodes the character references found in attribute
// values. The web UI's renderer decodes the same set, so both sides agree
// on what a README image's source is.
func decodeEntities(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	return entity.ReplaceAllStringFunc(s, func(e string) string {
		name := e[1 : len(e)-1]
		switch name {
		case "amp":
			return "&"
		case "lt":
			return "<"
		case "gt":
			return ">"
		case "quot":
			return `"`
		case "apos":
			return "'"
		}
		var n uint64
		var err error
		if name[1] == 'x' || name[1] == 'X' {
			n, err = strconv.ParseUint(name[2:], 16, 32)
		} else {
			n, err = strconv.ParseUint(name[1:], 10, 32)
		}
		if err != nil || n == 0 || n > 0x10FFFF || (n >= 0xD800 && n <= 0xDFFF) {
			return e
		}
		return string(rune(n))
	})
}

// closing returns the index of the bracket closing the one at open, or -1.
func closing(s string, open int, o, c byte) int {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case o:
			depth++
		case c:
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// imageRefs lists the image sources of a Markdown README as the web UI's
// renderer reads them: Markdown images (inline and by reference) and HTML
// <img> tags. The list may hold sources the renderer never shows (inside
// code blocks); it never misses one it shows.
func imageRefs(md string) []string {
	md = htmlComment.ReplaceAllString(md, "")
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] && len(out) < maxImages {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, tag := range imgTag.FindAllString(md, -1) {
		if src := strings.Fields(imgSrc(tag)); len(src) > 0 {
			add(src[0])
		}
	}
	refs := map[string]string{}
	for _, m := range refDef.FindAllStringSubmatch(md, -1) {
		refs[strings.ToLower(m[1])] = m[2]
	}
	for i := 0; i+1 < len(md); i++ {
		if md[i] == '\\' {
			i++
			continue
		}
		if md[i] != '!' || md[i+1] != '[' {
			continue
		}
		end := closing(md, i+1, '[', ']')
		if end < 0 {
			continue
		}
		text := md[i+2 : end]
		switch {
		case end+1 < len(md) && md[end+1] == '(':
			stop := closing(md, end+1, '(', ')')
			if stop < 0 {
				continue
			}
			dest := strings.TrimSpace(md[end+2 : stop])
			if strings.HasPrefix(dest, "<") {
				if j := strings.IndexByte(dest, '>'); j > 0 {
					add(dest[1:j])
					continue
				}
			}
			if f := strings.Fields(dest); len(f) > 0 {
				add(f[0])
			}
		case end+1 < len(md) && md[end+1] == '[':
			j := strings.IndexByte(md[end+2:], ']')
			if j < 0 {
				continue
			}
			label := md[end+2 : end+2+j]
			if label == "" {
				label = text
			}
			if href, ok := refs[strings.ToLower(label)]; ok {
				add(href)
			}
		default:
			if href, ok := refs[strings.ToLower(text)]; ok {
				add(href)
			}
		}
	}
	return out
}

// ---------------------------------------------------------------- resolution

// resolveImages records where each image of a Markdown README loads from.
func (x *extraction) resolveImages(ctx context.Context) {
	r := x.info.Readme
	if r == nil || r.Format != FormatMarkdown {
		return
	}
	images := map[string]Image{}
	for _, ref := range imageRefs(r.Content) {
		if img, ok := x.resolveImage(ctx, ref); ok {
			images[ref] = img
		}
	}
	if len(images) > 0 {
		r.Images = images
	}
}

// resolveImage resolves one README image source: absolute http(s) URLs are
// kept (repository file pages rewritten to their raw files); relative paths
// are looked up in the package archive next to the README, then in the
// source repository at the published commit.
func (x *extraction) resolveImage(ctx context.Context, ref string) (Image, bool) {
	if strings.HasPrefix(ref, "//") {
		ref = "https:" + ref
	}
	u, err := url.Parse(ref)
	if err != nil {
		return Image{}, false
	}
	if u.Scheme != "" {
		if (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil {
			return Image{URL: rawFileURL(u)}, true
		}
		return Image{}, false // data:, mailto:, javascript:, ...
	}
	if u.Host != "" || u.Path == "" {
		return Image{}, false
	}
	if !x.membersListed {
		x.listMembers(ctx)
	}
	rootRel := strings.HasPrefix(u.Path, "/")
	if member, ok := cleanRel(x.archiveRoot, x.readmeDir, u.Path, rootRel); ok {
		if name, ok := x.member(member); ok {
			return Image{Member: name}, true
		}
	}
	base := x.source.rawBase()
	if base == "" {
		return Image{}, false
	}
	p, ok := cleanRel("", x.source.dir, u.Path, rootRel)
	if !ok || p == "" {
		return Image{}, false
	}
	out := base + "/" + escapePath(p)
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return Image{URL: out}, true
}

// cleanRel joins a relative path to dir (or, when rootRel, to root) and
// cleans it; false when it leaves the root.
func cleanRel(root, dir, p string, rootRel bool) (string, bool) {
	base := dir
	if rootRel {
		base = root
	}
	joined := path.Clean("/" + strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(p, "/"))[1:]
	if root != "" && !strings.HasPrefix(joined+"/", strings.TrimSuffix(root, "/")+"/") {
		return "", false
	}
	return joined, joined != "" && joined != "."
}

func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// member returns the archive file called name (exact, else ignoring case).
func (x *extraction) member(name string) (string, bool) {
	if _, ok := x.members[name]; ok {
		return name, true
	}
	for m := range x.members {
		if strings.EqualFold(m, name) {
			return m, true
		}
	}
	return "", false
}

// listMembers lists the files of the package archive (npm tarballs; the
// .nupkg is listed while it is read).
func (x *extraction) listMembers(ctx context.Context) {
	x.membersListed = true
	if x.repo.Format != "npm" {
		return
	}
	tgz := x.assetBySuffix(ctx, ".tgz")
	if tgz == nil {
		return
	}
	rc, ok := x.open(ctx, tgz)
	if !ok {
		return
	}
	defer rc.Close()
	x.members = map[string]struct{}{}
	x.scanTarGz(ctx, rc, func(name string, _ io.Reader) bool {
		x.members[name] = struct{}{}
		if dir, file, nested := strings.Cut(name, "/"); nested && file == "package.json" && x.archiveRoot == "" {
			x.archiveRoot = dir + "/"
		}
		return false
	})
	if x.readmeDir == "" {
		x.readmeDir = x.archiveRoot
	}
}

// ---------------------------------------------------------------- repositories

// rawFileURL rewrites links to a file page on github.com or gitlab.com to
// the raw file, and image proxy links copied from github.com pages
// (camo.githubusercontent.com/<digest>/<hex URL>) to the image they wrap,
// which the proxy refuses to serve to anyone but github.com. Other URLs
// are returned as they are.
func rawFileURL(u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	segs := strings.Split(strings.TrimPrefix(u.EscapedPath(), "/"), "/")
	switch host {
	case "camo.githubusercontent.com":
		if len(segs) == 2 {
			if b, err := hex.DecodeString(segs[1]); err == nil {
				if inner, err := url.Parse(string(b)); err == nil && (inner.Scheme == "https" || inner.Scheme == "http") && inner.Host != "" &&
					inner.User == nil && !strings.EqualFold(inner.Hostname(), host) {
					return inner.String()
				}
			}
		}
	case "github.com", "www.github.com":
		// /owner/repo/blob/ref/path or /owner/repo/raw/ref/path
		if len(segs) >= 5 && (segs[2] == "blob" || segs[2] == "raw") {
			return "https://raw.githubusercontent.com/" + segs[0] + "/" + segs[1] + "/" + strings.Join(segs[3:], "/")
		}
	case "gitlab.com":
		// /group/.../project/-/blob/ref/path
		for i := 1; i+2 < len(segs); i++ {
			if segs[i] == "-" && segs[i+1] == "blob" {
				return "https://gitlab.com/" + strings.Join(segs[:i], "/") + "/-/raw/" + strings.Join(segs[i+2:], "/") + queryOf(u)
			}
		}
	}
	return u.String()
}

func queryOf(u *url.URL) string {
	if u.RawQuery == "" {
		return ""
	}
	return "?" + u.RawQuery
}

// parseRepoURL accepts the forms packages use for their repository:
// https URLs, git+https, git://, ssh "git@host:owner/repo.git" and the npm
// shorthands "github:owner/repo", "gitlab:owner/repo" and "owner/repo".
func parseRepoURL(raw string) (host string, segs []string) {
	s := strings.TrimSpace(raw)
	switch {
	case strings.HasPrefix(s, "github:"):
		s = "https://github.com/" + s[len("github:"):]
	case strings.HasPrefix(s, "gitlab:"):
		s = "https://gitlab.com/" + s[len("gitlab:"):]
	case strings.HasPrefix(s, "git@"):
		if h, p, ok := strings.Cut(s[len("git@"):], ":"); ok {
			s = "https://" + h + "/" + p
		}
	case !strings.Contains(s, ":") && strings.Count(s, "/") == 1:
		s = "https://github.com/" + s
	}
	s = strings.TrimPrefix(s, "git+")
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "", nil
	}
	for _, p := range strings.Split(strings.Trim(u.Path, "/"), "/") {
		if p != "" {
			segs = append(segs, p)
		}
	}
	if n := len(segs); n > 0 {
		segs[n-1] = strings.TrimSuffix(segs[n-1], ".git")
	}
	return strings.ToLower(u.Hostname()), segs
}

// rawBase is the URL raw files of the repository are served under at the
// published commit (else the default branch), for github.com and gitlab.com;
// "" for other hosts. A tree URL ("/tree/main/packages/x") gives the ref
// and directory when the package names none.
func (r sourceRepo) rawBase() string {
	host, segs := parseRepoURL(r.url)
	ref := r.commit
	if ref == "" {
		ref = "HEAD"
	}
	switch host {
	case "github.com", "www.github.com":
		if len(segs) < 2 {
			return ""
		}
		if len(segs) >= 4 && segs[2] == "tree" && r.commit == "" {
			ref = segs[3]
		}
		return "https://raw.githubusercontent.com/" + url.PathEscape(segs[0]) + "/" + url.PathEscape(segs[1]) + "/" + url.PathEscape(ref)
	case "gitlab.com":
		project := segs
		for i, s := range segs {
			if s == "-" {
				project = segs[:i]
				if i+2 < len(segs) && segs[i+1] == "tree" && r.commit == "" {
					ref = segs[i+2]
				}
				break
			}
		}
		if len(project) < 2 {
			return ""
		}
		return "https://gitlab.com/" + escapePath(strings.Join(project, "/")) + "/-/raw/" + url.PathEscape(ref)
	}
	return ""
}

// setSource keeps the first repository found.
func (x *extraction) setSource(repoURL, commit, dir string) {
	if x.source.url != "" || strings.TrimSpace(repoURL) == "" {
		return
	}
	if h, _ := parseRepoURL(repoURL); h == "" {
		return
	}
	x.source = sourceRepo{url: strings.TrimSpace(repoURL), commit: strings.TrimSpace(commit), dir: strings.Trim(strings.TrimSpace(dir), "/")}
}
