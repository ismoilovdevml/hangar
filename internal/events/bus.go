package events

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// EventSubjectPrefix prefixes event subjects: "hangar.events.component.published".
const EventSubjectPrefix = "hangar.events."

// Bus is a core.EventBus over core NATS publish/subscribe. Delivery is
// at-most-once to currently connected subscribers, which suits live feeds and
// cache invalidation; durable work belongs on the JobQueue.
type Bus struct {
	nc  *nats.Conn
	log *slog.Logger
}

var _ core.EventBus = (*Bus)(nil)

// NewBus returns a bus on nc.
func NewBus(nc *nats.Conn, log *slog.Logger) *Bus {
	if log == nil {
		log = slog.Default()
	}
	return &Bus{nc: nc, log: log}
}

// Publish implements core.EventBus.
func (b *Bus) Publish(_ context.Context, e core.Event) error {
	if e.Type == "" {
		return fmt.Errorf("event without type: %w", core.ErrInvalid)
	}
	if e.ID == "" {
		e.ID = newID()
	}
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("encode event: %w", err)
	}
	if err := b.nc.Publish(EventSubjectPrefix+e.Type, data); err != nil {
		return fmt.Errorf("publish %s: %w", e.Type, err)
	}
	return nil
}

// Subscribe implements core.EventBus.
func (b *Bus) Subscribe(ctx context.Context, pattern string, h func(context.Context, core.Event)) (core.Subscription, error) {
	sub, err := b.nc.Subscribe(EventSubjectPrefix+pattern, func(m *nats.Msg) {
		var e core.Event
		if err := json.Unmarshal(m.Data, &e); err != nil {
			b.log.Warn("dropping malformed event", "subject", m.Subject, "error", err)
			return
		}
		h(context.WithoutCancel(ctx), e)
	})
	if err != nil {
		return nil, fmt.Errorf("subscribe %s: %w", pattern, err)
	}
	return sub, nil
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
