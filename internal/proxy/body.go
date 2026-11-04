package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// bodyRequest is what a body needs to re-request its remainder.
type bodyRequest struct {
	method  string
	target  *url.URL
	req     core.UpstreamRequest
	primary bool
}

// upstreamBody is a remote response body with two protections for large
// transfers, which have no overall deadline:
//
//   - a stall timeout: when one read waits longer than bodyTimeout for data,
//     the request is cancelled instead of hanging until the client gives up;
//   - resumption: when the transfer breaks (stall, reset connection,
//     truncation) and the remote advertised Accept-Ranges with a strong
//     validator, the rest is requested with Range + If-Range and the stream
//     continues where it stopped, up to maxResumes times. Callers verify
//     digests as before, so a remote that changes the content in between
//     cannot slip mixed bytes through.
//
// It also holds the request's concurrency slot until closed. Reads and Close
// come from one goroutine (the flight group's broadcaster).
type upstreamBody struct {
	u       *upstream
	ctx     context.Context // the fetch context; resumes run on it
	br      bodyRequest
	release func()

	cur     transfer
	timeout time.Duration

	total     int64  // Content-Length; -1 unknown
	read      int64  // bytes delivered so far
	validator string // If-Range value; "" = not resumable
	resumes   int
	pending   error // read error deferred after returning data
	closeOnce sync.Once
}

// transfer is one response body being read.
type transfer struct {
	rc      io.ReadCloser
	cancel  context.CancelFunc
	timer   *time.Timer // nil without a stall timeout
	stalled *atomic.Bool
}

func (u *upstream) newBody(ctx context.Context, br bodyRequest, resp *http.Response, cancel context.CancelFunc, release func()) io.ReadCloser {
	b := &upstreamBody{u: u, ctx: ctx, br: br, release: release, timeout: u.bodyTimeout, total: resp.ContentLength}
	if br.method == http.MethodGet && resp.StatusCode == http.StatusOK && resp.ContentLength > 0 && !resp.Uncompressed &&
		resp.Header.Get("Content-Encoding") == "" && acceptsByteRanges(resp.Header) {
		if et := resp.Header.Get("ETag"); et != "" && !strings.HasPrefix(et, "W/") {
			b.validator = et
		} else if lm := resp.Header.Get("Last-Modified"); lm != "" {
			if _, err := http.ParseTime(lm); err == nil {
				b.validator = lm
			}
		}
	}
	b.cur = b.newTransfer(resp.Body, cancel)
	return b
}

func acceptsByteRanges(h http.Header) bool {
	for _, v := range h.Values("Accept-Ranges") {
		for f := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(f), "bytes") {
				return true
			}
		}
	}
	return false
}

func (b *upstreamBody) newTransfer(rc io.ReadCloser, cancel context.CancelFunc) transfer {
	a := transfer{rc: rc, cancel: cancel, stalled: &atomic.Bool{}}
	if b.timeout > 0 {
		stalled := a.stalled
		a.timer = time.AfterFunc(b.timeout, func() {
			stalled.Store(true)
			cancel()
		})
		a.timer.Stop()
	}
	return a
}

func (a transfer) close() {
	if a.timer != nil {
		a.timer.Stop()
	}
	_ = a.rc.Close()
	a.cancel()
}

func (b *upstreamBody) Read(p []byte) (int, error) {
	for {
		if err := b.pending; err != nil {
			b.pending = nil
			if err := b.recover(err); err != nil {
				return 0, err
			}
			continue
		}
		if b.cur.timer != nil {
			b.cur.timer.Reset(b.timeout)
		}
		n, err := b.cur.rc.Read(p)
		if b.cur.timer != nil {
			b.cur.timer.Stop()
		}
		b.read += int64(n)
		if errors.Is(err, io.EOF) && b.total >= 0 && b.read < b.total {
			err = io.ErrUnexpectedEOF
		}
		if err == nil || errors.Is(err, io.EOF) {
			return n, err
		}
		if n > 0 {
			b.pending = err
			return n, nil
		}
		if err := b.recover(err); err != nil {
			return 0, err
		}
	}
}

// recover handles a broken transfer: it resumes when possible and returns
// the error to report otherwise.
func (b *upstreamBody) recover(cause error) error {
	if b.cur.stalled.Load() {
		cause = fmt.Errorf("no data for %s: %w", b.timeout, cause)
	}
	fail := func(err error) error {
		b.u.log.Warn("upstream transfer failed", "repository", b.u.name, "url", b.br.target.Redacted(),
			"read", b.read, "size", b.total, "resumes", b.resumes, "error", err)
		return fmt.Errorf("upstream %s: read %s: %w: %w", b.u.name, b.br.target.Redacted(), err, core.ErrUpstream)
	}
	if b.ctx.Err() != nil {
		return fmt.Errorf("upstream %s: read %s: %w", b.u.name, b.br.target.Redacted(), cause) // every caller left
	}
	if b.validator == "" || b.resumes >= b.u.maxResumes {
		return fail(cause)
	}
	if err := b.resume(); err != nil {
		return fail(fmt.Errorf("%w; resume failed: %w", cause, err))
	}
	return nil
}

// resume requests the remainder of the body.
func (b *upstreamBody) resume() error {
	b.cur.close()
	b.cur = transfer{rc: http.NoBody, cancel: func() {}, stalled: &atomic.Bool{}}
	if err := b.u.check(b.ctx, b.br.target); err != nil {
		return err
	}
	req := b.br.req
	req.Header = req.Header.Clone()
	if req.Header == nil {
		req.Header = http.Header{}
	}
	req.Header.Set("Range", "bytes="+strconv.FormatInt(b.read, 10)+"-")
	req.Header.Set("If-Range", b.validator)
	req.Conditional = core.Conditional{}
	actx, cancel := context.WithCancel(b.ctx)
	resp, err := b.u.attempt(actx, http.MethodGet, b.br.target, req, b.br.primary)
	if err != nil {
		cancel()
		return err
	}
	if resp.StatusCode != http.StatusPartialContent || !rangeContinues(resp.Header.Get("Content-Range"), b.read, b.total) {
		discard(resp)
		cancel()
		return fmt.Errorf("remote answered %d (Content-Range %q) to a range request from byte %d", resp.StatusCode, resp.Header.Get("Content-Range"), b.read)
	}
	b.resumes++
	resumesTotal.WithLabelValues(b.u.name).Inc()
	b.u.log.Info("upstream transfer resumed", "repository", b.u.name, "url", b.br.target.Redacted(), "from", b.read, "size", b.total)
	b.cur = b.newTransfer(resp.Body, cancel)
	return nil
}

// rangeContinues checks a Content-Range ("bytes 100-999/1000") against the
// expected start and total size.
func rangeContinues(cr string, start, total int64) bool {
	spec, ok := strings.CutPrefix(strings.TrimSpace(cr), "bytes ")
	if !ok {
		return false
	}
	rng, size, ok := strings.Cut(spec, "/")
	if !ok {
		return false
	}
	first, last, ok := strings.Cut(rng, "-")
	if !ok {
		return false
	}
	f, err1 := strconv.ParseInt(first, 10, 64)
	l, err2 := strconv.ParseInt(last, 10, 64)
	if err1 != nil || err2 != nil || f != start || l != total-1 {
		return false
	}
	if size == "*" {
		return true
	}
	s, err := strconv.ParseInt(size, 10, 64)
	return err == nil && s == total
}

func (b *upstreamBody) Close() error {
	b.closeOnce.Do(func() {
		b.cur.close()
		b.release()
	})
	return nil
}
