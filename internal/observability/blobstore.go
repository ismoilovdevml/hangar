package observability

import (
	"context"
	"errors"
	"io"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Blob store operations.
const (
	blobOpPut    = "put"
	blobOpGet    = "get"
	blobOpStat   = "stat"
	blobOpDelete = "delete"
)

// InstrumentBlobStores wraps next so every store it returns records
// operation latency, outcome and bytes written per driver, and a span per
// operation. driverOf names a store's driver ("fs", "s3"); it is called once
// per lookup and should be a cheap type switch. Reads are timed until the
// blob is open, not until the client finished downloading it, and the
// returned reader is the driver's own (so sendfile and range reads work).
func InstrumentBlobStores(next core.BlobStores, m *Metrics, driverOf func(core.BlobStore) string) core.BlobStores {
	if m == nil {
		return next
	}
	return instrumentedStores{next: next, m: m, driverOf: driverOf}
}

type instrumentedStores struct {
	next     core.BlobStores
	m        *Metrics
	driverOf func(core.BlobStore) string
}

func (s instrumentedStores) ForRepository(ctx context.Context, repo *core.Repository) (core.BlobStore, error) {
	st, err := s.next.ForRepository(ctx, repo)
	if err != nil {
		return nil, err
	}
	driver := labelUnknown
	if s.driverOf != nil {
		driver = driverLabel(s.driverOf(st))
	}
	return &instrumentedStore{BlobStore: st, m: s.m, driver: driver}, nil
}

func driverLabel(d string) string {
	switch d {
	case "fs", "s3":
		return d
	case "":
		return labelUnknown
	}
	return labelOther
}

type instrumentedStore struct {
	core.BlobStore
	m      *Metrics
	driver string
}

func (s *instrumentedStore) start(ctx context.Context, op string) (context.Context, trace.Span, time.Time) {
	ctx, span := tracer().Start(ctx, "blobstore."+op, trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("hangar.blobstore.driver", s.driver)))
	return ctx, span, time.Now()
}

func (s *instrumentedStore) finish(span trace.Span, op string, start time.Time, err error) {
	result := "ok"
	switch {
	case err == nil:
	case errors.Is(err, core.ErrNotFound):
		result = "not_found"
	default:
		result = "error"
		span.RecordError(err)
		span.SetStatus(codes.Error, op+" failed")
	}
	s.m.BlobOps.WithLabelValues(s.driver, op, result).Inc()
	s.m.BlobDuration.WithLabelValues(s.driver, op).Observe(time.Since(start).Seconds())
	span.End()
}

func (s *instrumentedStore) Put(ctx context.Context, r io.Reader) (core.Digest, int64, error) {
	ctx, span, start := s.start(ctx, blobOpPut)
	d, n, err := s.BlobStore.Put(ctx, r)
	if n > 0 {
		s.m.BlobPutBytes.WithLabelValues(s.driver).Add(float64(n))
	}
	span.SetAttributes(attribute.Int64("hangar.blob.size", n))
	s.finish(span, blobOpPut, start, err)
	return d, n, err
}

func (s *instrumentedStore) Get(ctx context.Context, d core.Digest) (io.ReadSeekCloser, error) {
	ctx, span, start := s.start(ctx, blobOpGet)
	rc, err := s.BlobStore.Get(ctx, d)
	s.finish(span, blobOpGet, start, err)
	return rc, err
}

func (s *instrumentedStore) Stat(ctx context.Context, d core.Digest) (core.BlobInfo, error) {
	ctx, span, start := s.start(ctx, blobOpStat)
	info, err := s.BlobStore.Stat(ctx, d)
	s.finish(span, blobOpStat, start, err)
	return info, err
}

func (s *instrumentedStore) Delete(ctx context.Context, d core.Digest) error {
	ctx, span, start := s.start(ctx, blobOpDelete)
	err := s.BlobStore.Delete(ctx, d)
	s.finish(span, blobOpDelete, start, err)
	return err
}

// Unwrap returns the driver's store, for code that needs its optional
// capabilities.
func (s *instrumentedStore) Unwrap() core.BlobStore { return s.BlobStore }
