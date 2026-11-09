package importer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// local sends requests to Hangar's own package protocols in process, as the
// principal of the run (see Authenticator).
type local struct {
	h       http.Handler
	host    string
	baseURL string
	caller  *core.Principal
}

func (s *Service) newLocal(ctx context.Context) *local {
	host := "localhost"
	if u, err := url.Parse(s.opts.BaseURL); err == nil && u.Host != "" {
		host = u.Host
	}
	return &local{h: s.opts.Handler, host: host, baseURL: s.opts.BaseURL, caller: core.PrincipalFrom(ctx)}
}

// localResp is the answer to an in-process request. Body holds at most the
// keep bytes asked for; Size counts every byte written.
type localResp struct {
	Status    int
	Header    http.Header
	Body      []byte
	Size      int64
	Truncated bool
}

func (r *localResp) ok() bool { return r.Status/100 == 2 }

// message is the error text of a non-2xx answer.
func (r *localResp) message() string {
	msg := strings.TrimSpace(string(r.Body))
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	if msg == "" {
		msg = http.StatusText(r.Status)
	}
	return fmt.Sprintf("%d %s", r.Status, msg)
}

type recorder struct {
	header http.Header
	status int
	keep   int64
	buf    bytes.Buffer
	size   int64
	trunc  bool
}

func (w *recorder) Header() http.Header { return w.header }

func (w *recorder) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}

func (w *recorder) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.size += int64(len(p))
	if room := w.keep - int64(w.buf.Len()); room > 0 {
		if int64(len(p)) > room {
			w.buf.Write(p[:room])
			w.trunc = true
		} else {
			w.buf.Write(p)
		}
	} else if len(p) > 0 {
		w.trunc = true
	}
	return len(p), nil
}

func (w *recorder) Flush() {}

// do sends method path (an absolute path such as /repository/npm/lodash or
// /v2/...) with body and returns the answer, keeping up to keep body bytes.
func (l *local) do(ctx context.Context, method, path string, hdr http.Header, body io.Reader, size int64, keep int64) (*localResp, error) {
	if body == nil {
		body = http.NoBody
	}
	req, err := http.NewRequestWithContext(withCaller(ctx, l.caller), method, "http://"+l.host+path, body)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", path, err)
	}
	req.RemoteAddr = "127.0.0.1:0"
	req.Header.Set("User-Agent", "hangar-import")
	for k, v := range hdr {
		req.Header[k] = v
	}
	if size >= 0 {
		req.ContentLength = size
	}
	w := &recorder{header: http.Header{}, keep: keep}
	l.h.ServeHTTP(w, req)
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if err := ctx.Err(); err != nil {
		return nil, context.Cause(ctx)
	}
	return &localResp{Status: w.status, Header: w.header, Body: w.buf.Bytes(), Size: w.size, Truncated: w.trunc}, nil
}

// get fetches path, keeping up to keep body bytes.
func (l *local) get(ctx context.Context, path string, hdr http.Header, keep int64) (*localResp, error) {
	return l.do(ctx, http.MethodGet, path, hdr, nil, -1, keep)
}

// repoPath is the path of rel in repository repo.
func repoPath(repo, rel string) string {
	return "/repository/" + url.PathEscape(repo) + "/" + strings.TrimPrefix(rel, "/")
}

// pathOf maps a URL Hangar generated (absolute with base_url, or a path) to
// a path of the handler. Links to other hosts yield "".
func (l *local) pathOf(ref string) string {
	u, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	if u.IsAbs() {
		if l.baseURL != "" {
			b, err := url.Parse(l.baseURL)
			if err == nil && !strings.EqualFold(b.Host, u.Host) {
				return ""
			}
		} else if !strings.EqualFold(u.Host, l.host) {
			return ""
		}
	}
	p := u.EscapedPath()
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	return p
}

// publishCtx asks the component service to keep the source's publish time.
func publishCtx(ctx context.Context, at time.Time) context.Context {
	return core.WithPublishedAt(ctx, at)
}
