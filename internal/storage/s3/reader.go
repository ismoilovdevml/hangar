package s3

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/minio/minio-go/v7"
)

// objectReader is an io.ReadSeekCloser over one S3 object. Seek only moves
// the offset; the next Read opens a ranged GET from there. Every GET carries
// If-Match with the ETag seen by Get, so a reader never mixes bytes from two
// object versions (blobs are immutable, so this only trips on tampering).
type objectReader struct {
	ctx    context.Context
	core   *minio.Core
	bucket string
	key    string
	size   int64
	etag   string

	off    int64
	body   io.ReadCloser
	closed bool
}

func (o *objectReader) Read(p []byte) (int, error) {
	if o.closed {
		return 0, errors.New("s3 blob reader: read after close")
	}
	if o.off >= o.size {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	if o.body == nil {
		opts := minio.GetObjectOptions{}
		if o.etag != "" {
			if err := opts.SetMatchETag(o.etag); err != nil {
				return 0, fmt.Errorf("s3 blob reader: %w", err)
			}
		}
		if o.off > 0 {
			if err := opts.SetRange(o.off, 0); err != nil {
				return 0, fmt.Errorf("s3 blob reader: %w", err)
			}
		}
		body, _, _, err := o.core.GetObject(o.ctx, o.bucket, o.key, opts)
		if err != nil {
			return 0, fmt.Errorf("s3 blob reader: get %s at %d: %w", o.key, o.off, err)
		}
		o.body = body
	}
	n, err := o.body.Read(p)
	o.off += int64(n)
	if errors.Is(err, io.EOF) && o.off < o.size {
		return n, fmt.Errorf("s3 blob reader: %s: %w", o.key, io.ErrUnexpectedEOF)
	}
	return n, err
}

func (o *objectReader) Seek(offset int64, whence int) (int64, error) {
	if o.closed {
		return 0, errors.New("s3 blob reader: seek after close")
	}
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = o.off + offset
	case io.SeekEnd:
		abs = o.size + offset
	default:
		return 0, fmt.Errorf("s3 blob reader: invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, errors.New("s3 blob reader: negative position")
	}
	if abs != o.off && o.body != nil {
		_ = o.body.Close()
		o.body = nil
	}
	o.off = abs
	return abs, nil
}

func (o *objectReader) Close() error {
	if o.closed {
		return nil
	}
	o.closed = true
	if o.body != nil {
		err := o.body.Close()
		o.body = nil
		return err
	}
	return nil
}
