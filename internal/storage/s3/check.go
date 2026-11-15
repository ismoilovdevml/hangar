package s3

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"

	"github.com/minio/minio-go/v7"
)

// CheckStep is one step of Check: its name, how long it took and why it
// failed (nil when it passed).
type CheckStep struct {
	Name    string
	Latency time.Duration
	Err     error
}

// Check steps, in order.
const (
	CheckBucket = "bucket"
	CheckWrite  = "write"
	CheckRead   = "read"
	CheckDelete = "delete"
)

// Check tests the store end to end without touching blobs: the bucket is
// reachable with the credentials, and an object under tmp/ can be written,
// read back and deleted (what storing a blob needs). It stops at the first
// step that fails, except that a written object is always deleted again.
func (s *Store) Check(ctx context.Context) []CheckStep {
	var steps []CheckStep
	run := func(name string, f func() error) bool {
		start := time.Now()
		err := f()
		steps = append(steps, CheckStep{Name: name, Latency: time.Since(start), Err: err})
		return err == nil
	}
	endpoint := s.core.EndpointURL().String()

	if !run(CheckBucket, func() error {
		ok, err := s.core.BucketExists(ctx, s.bucket)
		if err != nil {
			return fmt.Errorf("bucket %q at %s: %s", s.bucket, endpoint, describe(err))
		}
		if !ok {
			return fmt.Errorf("bucket %q not found at %s (NoSuchBucket)", s.bucket, endpoint)
		}
		return nil
	}) {
		return steps
	}

	key := s.newTmpKey() + "-connection-check"
	payload := []byte("hangar connection check\n")
	written := run(CheckWrite, func() error {
		_, err := s.core.PutObject(ctx, s.bucket, key, bytes.NewReader(payload), int64(len(payload)), "",
			s.payloadSHA256(payload), minio.PutObjectOptions{ContentType: contentType, DisableContentSha256: true})
		if err != nil {
			return fmt.Errorf("write %s: %s", key, describe(err))
		}
		return nil
	})
	if written {
		run(CheckRead, func() error {
			obj, _, _, err := s.core.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
			if err != nil {
				return fmt.Errorf("read %s: %s", key, describe(err))
			}
			defer func() { _ = obj.Close() }()
			got, err := io.ReadAll(io.LimitReader(obj, int64(len(payload))+1))
			if err != nil {
				return fmt.Errorf("read %s: %s", key, describe(err))
			}
			if !bytes.Equal(got, payload) {
				return fmt.Errorf("read %s: the object came back changed", key)
			}
			return nil
		})
	}
	if written {
		run(CheckDelete, func() error {
			if err := s.core.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
				return fmt.Errorf("delete %s: %s", key, describe(err))
			}
			return nil
		})
	}
	return steps
}

// describe renders an S3 error as "message (Code)", or the error itself
// for transport failures.
func describe(err error) string {
	if resp := minio.ToErrorResponse(err); resp.Code != "" {
		msg := resp.Message
		if msg == "" {
			msg = resp.Code
		}
		return fmt.Sprintf("%s (%s)", msg, resp.Code)
	}
	return err.Error()
}
