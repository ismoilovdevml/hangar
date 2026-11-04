package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ismoilovdevml/hangar/internal/api/catalog"
	"github.com/ismoilovdevml/hangar/internal/core"
)

// liveSubjects are the EventBus patterns forwarded to /api/v1/events.
var liveSubjects = []string{"component.*", "task.*", core.EventUpstreamStatus}

const (
	replaySize   = 256 // events kept for Last-Event-ID resume
	clientBuffer = 64  // per-connection queue; a client that falls behind is disconnected
	authzTTL     = time.Minute
)

// liveEvent is one SSE message, already rendered for the UI.
type liveEvent struct {
	ID         string
	Type       string
	Repository string
	Data       []byte
}

type liveClient struct {
	ch    chan liveEvent
	lost  chan struct{}
	types map[string]bool // nil = all
	once  sync.Once
}

func (c *liveClient) drop() { c.once.Do(func() { close(c.lost) }) }

// hub fans bus events out to SSE connections with one subscription per
// pattern for the whole process.
type hub struct {
	render func(context.Context, core.Event) liveEvent
	log    *slog.Logger

	mu      sync.Mutex
	clients map[*liveClient]struct{}
	ring    []liveEvent
	subs    []core.Subscription
}

func newHub(ctx context.Context, bus core.EventBus, render func(context.Context, core.Event) liveEvent, log *slog.Logger) (*hub, error) {
	h := &hub{render: render, log: log, clients: map[*liveClient]struct{}{}}
	for _, p := range liveSubjects {
		sub, err := bus.Subscribe(context.WithoutCancel(ctx), p, h.publish)
		if err != nil {
			h.close()
			return nil, fmt.Errorf("subscribe %s: %w", p, err)
		}
		h.subs = append(h.subs, sub)
	}
	return h, nil
}

func (h *hub) publish(ctx context.Context, e core.Event) {
	ev := h.render(ctx, e)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ring = append(h.ring, ev)
	if len(h.ring) > replaySize {
		h.ring = h.ring[len(h.ring)-replaySize:]
	}
	for c := range h.clients {
		if c.types != nil && !c.types[ev.Type] {
			continue
		}
		select {
		case c.ch <- ev:
		default:
			c.drop() // too slow: it reconnects with Last-Event-ID
		}
	}
}

// add registers a client and returns the events after lastID still in the
// replay window (none if lastID is unknown).
func (h *hub) add(types map[string]bool, lastID string) (*liveClient, []liveEvent) {
	c := &liveClient{ch: make(chan liveEvent, clientBuffer), lost: make(chan struct{}), types: types}
	h.mu.Lock()
	defer h.mu.Unlock()
	var replay []liveEvent
	if lastID != "" {
		for i, ev := range h.ring {
			if ev.ID == lastID {
				for _, r := range h.ring[i+1:] {
					if types == nil || types[r.Type] {
						replay = append(replay, r)
					}
				}
				break
			}
		}
	}
	h.clients[c] = struct{}{}
	return c, replay
}

func (h *hub) remove(c *liveClient) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

func (h *hub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.subs {
		_ = s.Unsubscribe()
	}
	h.subs = nil
	for c := range h.clients {
		c.drop()
	}
}

func (a *API) eventHub(ctx context.Context) (*hub, error) {
	a.hubMu.Lock()
	defer a.hubMu.Unlock()
	if a.hub != nil {
		return a.hub, nil
	}
	h, err := newHub(ctx, a.Deps.Events, a.renderEvent, a.logger())
	if err != nil {
		return nil, err
	}
	a.hub = h
	return h, nil
}

// renderEvent converts a bus event into the LiveEvent data the UI expects.
func (a *API) renderEvent(ctx context.Context, e core.Event) liveEvent {
	ev := liveEvent{ID: e.ID, Type: e.Type, Repository: e.Repository, Data: e.Data}
	if ev.ID == "" {
		var b [8]byte
		_, _ = rand.Read(b[:])
		ev.ID = hex.EncodeToString(b[:])
	}
	var comp struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		Version    string `json:"version"`
		Repository string `json:"repository"`
		Reason     string `json:"reason"`
	}
	_ = json.Unmarshal(e.Data, &comp)
	if comp.Repository == "" {
		comp.Repository = e.Repository
	}
	var out any
	switch e.Type {
	case core.EventComponentPublished:
		out = a.publishedEventData(ctx, e, comp.ID)
	case core.EventComponentDeleted:
		out = map[string]string{"id": comp.ID, "repository": comp.Repository}
	case core.EventComponentQuarantined, core.EventComponentReleased:
		out = comp
	case core.EventUpstreamStatus:
		var st struct {
			State    string `json:"state"`
			Previous string `json:"previous"`
			URL      string `json:"url"`
		}
		_ = json.Unmarshal(e.Data, &st)
		out = map[string]string{"name": e.Repository, "url": st.URL, "state": st.State, "previous": st.Previous}
	}
	if out != nil {
		ev.Data, _ = json.Marshal(out)
	}
	if len(ev.Data) == 0 {
		ev.Data = []byte("{}")
	}
	return ev
}

// publishedEventData renders component.published as PublishedComponent,
// from the catalog when available (scan summary, publisher kind).
func (a *API) publishedEventData(ctx context.Context, e core.Event, id string) any {
	if a.Catalog != nil && catalog.ValidID(id) {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if d, err := a.Catalog.Component(cctx, id); err == nil {
			return toPublished(&d.ComponentRow)
		}
	}
	var c core.Component
	if err := json.Unmarshal(e.Data, &c); err != nil {
		return nil
	}
	if c.Repository == "" {
		c.Repository = e.Repository
	}
	state := "pending"
	if c.Quarantined {
		state = "quarantined"
	}
	return toPublished(&catalog.ComponentRow{Component: c, PublisherKind: "user", Scan: catalog.ScanSummary{State: state}})
}

type authzCache struct {
	a       *API
	ctx     context.Context
	entries map[string]authzEntry
}

type authzEntry struct {
	ok bool
	at time.Time
}

// allowed applies RBAC to one event: repository events need repo read,
// task events system:tasks read.
func (c *authzCache) allowed(ev liveEvent) bool {
	var res core.Resource
	switch {
	case strings.HasPrefix(ev.Type, "task."):
		res = core.SystemResource(core.SystemTasks)
	case ev.Repository != "":
		res = core.RepoResource(ev.Repository)
	default:
		return false
	}
	key := res.String()
	if e, ok := c.entries[key]; ok && time.Since(e.at) < authzTTL {
		return e.ok
	}
	ok := c.a.authorize(c.ctx, core.ActionRead, res) == nil
	c.entries[key] = authzEntry{ok: ok, at: time.Now()}
	return ok
}

func oneLine(s string) string { return strings.NewReplacer("\r", "", "\n", "").Replace(s) }

func writeEvent(w http.ResponseWriter, ev liveEvent) error {
	_, err := fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", oneLine(ev.ID), oneLine(ev.Type), oneLine(string(ev.Data)))
	return err
}

// events streams Server-Sent Events.
func (a *API) events(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := requireUser(ctx); err != nil {
		a.writeErr(w, r, err)
		return
	}
	if a.Deps.Events == nil {
		a.writeErr(w, r, notImplemented("event stream"))
		return
	}
	var types map[string]bool
	if s := r.URL.Query().Get("types"); s != "" {
		types = map[string]bool{}
		for _, t := range strings.Split(s, ",") {
			if t = strings.TrimSpace(t); t != "" {
				types[t] = true
			}
		}
	}
	h, err := a.eventHub(ctx)
	if err != nil {
		a.logger().ErrorContext(ctx, "event stream unavailable", "error", err)
		WriteError(w, http.StatusServiceUnavailable, "unavailable", "event stream unavailable")
		return
	}
	client, replay := h.add(types, r.Header.Get("Last-Event-ID"))
	defer h.remove(client)

	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // nginx: do not buffer the stream
	w.WriteHeader(http.StatusOK)
	if _, err := fmt.Fprint(w, "retry: 3000\n\n"); err != nil {
		return
	}
	authz := &authzCache{a: a, ctx: ctx, entries: map[string]authzEntry{}}
	for _, ev := range replay {
		if authz.allowed(ev) {
			if writeEvent(w, ev) != nil {
				return
			}
		}
	}
	if err := rc.Flush(); err != nil {
		if !errors.Is(err, http.ErrNotSupported) {
			return
		}
	}
	beat := a.EventsHeartbeat
	if beat <= 0 {
		beat = 15 * time.Second
	}
	tick := time.NewTicker(beat)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-client.lost:
			return
		case <-tick.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
		case ev := <-client.ch:
			if !authz.allowed(ev) {
				continue
			}
			if writeEvent(w, ev) != nil {
				return
			}
		}
		if err := rc.Flush(); err != nil {
			return
		}
	}
}
