package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// flightGroup collapses concurrent identical upstream requests into one.
//
// Unlike x/sync/singleflight it shares a streaming body: every caller that
// joined before the response headers arrived gets its own io.Pipe fed from the
// single upstream body, so nothing is buffered beyond one read chunk. The
// stream advances at the pace of the slowest reader; a caller whose context
// ends, or who closes its body, is detached and no longer holds the others
// back. The upstream request runs on a context detached from any single
// caller and is cancelled only when every caller has gone.
type flightGroup struct {
	mu    sync.Mutex
	calls map[string]*flight
}

type flight struct {
	key    string
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	res    *core.UpstreamResponse
	err    error
	subs   []*subscriber
	active int  // subscribers that have not left; guarded by flightGroup.mu
	sealed bool // headers arrived: no more joiners
}

type subscriber struct {
	pr   *io.PipeReader
	pw   *io.PipeWriter
	left bool
}

// fetchFunc performs the upstream request. The returned response's Body is
// owned by the flight group.
type fetchFunc func(ctx context.Context) (*core.UpstreamResponse, error)

// do runs fn once for all concurrent callers with the same key.
func (g *flightGroup) do(ctx context.Context, key string, fn fetchFunc) (*core.UpstreamResponse, error) {
	g.mu.Lock()
	if g.calls == nil {
		g.calls = map[string]*flight{}
	}
	f, ok := g.calls[key]
	if !ok {
		upCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		f = &flight{key: key, ctx: upCtx, cancel: cancel, done: make(chan struct{})}
		g.calls[key] = f
	}
	pr, pw := io.Pipe()
	sub := &subscriber{pr: pr, pw: pw}
	f.subs = append(f.subs, sub)
	f.active++
	g.mu.Unlock()

	if !ok {
		go g.run(f, fn)
	}

	select {
	case <-f.done:
	case <-ctx.Done():
		g.leave(f, sub, ctx.Err())
		return nil, ctx.Err()
	}
	if f.err != nil {
		g.leave(f, sub, nil)
		return nil, f.err
	}

	res := *f.res
	res.Header = f.res.Header.Clone()
	if f.res.Body == http.NoBody {
		g.leave(f, sub, nil)
		res.Body = http.NoBody
		return &res, nil
	}
	stop := context.AfterFunc(ctx, func() { g.leave(f, sub, ctx.Err()) })
	res.Body = &subBody{PipeReader: sub.pr, close: func() {
		stop()
		g.leave(f, sub, nil)
	}}
	return &res, nil
}

// leave detaches sub; its pending reads fail with cause (io.ErrClosedPipe if
// nil). When no subscriber is left the upstream request is cancelled and, if
// the response has not arrived yet, the flight is forgotten so a later caller
// starts afresh.
func (g *flightGroup) leave(f *flight, sub *subscriber, cause error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if sub.left {
		return
	}
	sub.left = true
	f.active--
	_ = sub.pr.CloseWithError(cause)
	if f.active > 0 {
		return
	}
	f.cancel()
	if !f.sealed && g.calls[f.key] == f {
		delete(g.calls, f.key)
	}
}

func (g *flightGroup) run(f *flight, fn fetchFunc) {
	res, err := fn(f.ctx)

	g.mu.Lock()
	if g.calls[f.key] == f {
		delete(g.calls, f.key)
	}
	f.sealed = true
	subs := make([]*subscriber, 0, len(f.subs))
	for _, s := range f.subs {
		if !s.left {
			subs = append(subs, s)
		}
	}
	g.mu.Unlock()

	if err == nil && len(subs) == 0 {
		err = context.Canceled
	}
	if err != nil {
		if res != nil && res.Body != nil {
			_ = res.Body.Close()
		}
		f.err = err
		f.cancel()
		close(f.done)
		return
	}
	body := res.Body
	if body == nil {
		body = http.NoBody
	}
	f.res = res
	if body == http.NoBody {
		f.res.Body = http.NoBody
		f.cancel()
		close(f.done)
		return
	}
	f.res.Body = nil
	close(f.done)
	broadcast(body, subs)
	f.cancel()
}

// broadcast copies body to every subscriber pipe, dropping readers that went away.
func broadcast(body io.ReadCloser, subs []*subscriber) {
	defer func() { _ = body.Close() }()
	buf := make([]byte, 32<<10)
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			alive := 0
			for _, s := range subs {
				if s.pw == nil {
					continue
				}
				if _, werr := s.pw.Write(buf[:n]); werr != nil {
					s.pw = nil // reader closed or its context ended
					continue
				}
				alive++
			}
			if alive == 0 {
				return
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				rerr = nil
			}
			for _, s := range subs {
				if s.pw != nil {
					_ = s.pw.CloseWithError(rerr)
				}
			}
			return
		}
	}
}

// subBody is one caller's view of a shared body.
type subBody struct {
	*io.PipeReader
	once  sync.Once
	close func()
}

func (b *subBody) Close() error {
	b.once.Do(b.close)
	return nil
}
