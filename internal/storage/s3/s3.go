// Package s3 is a core.BlobStore on S3-compatible object storage: AWS S3,
// MinIO, Ceph RGW, and any other service that speaks the S3 API.
//
// Layout under the configured bucket and prefix:
//
//	blobs/sha256/<hex[0:2]>/<hex[2:4]>/<hex>   committed, immutable blobs
//	tmp/<id>                                   in-flight multipart uploads
//
// Put reads the stream one part at a time (Config.PartSize, default 16 MiB)
// while hashing it, so memory per upload is bounded by one part, never the
// whole blob:
//
//   - Content that fits in one part is hashed before it is sent, so it is
//     written straight to its final key with a single PUT (atomic in S3).
//   - Larger content is streamed as a multipart upload to a temp key under
//     tmp/. Once complete, the digest is known: if the final key already
//     exists the temp object is dropped (deduplication), otherwise it is
//     copied server side to the final key and then deleted.
//
// Readers never observe partial blobs, because S3 objects become visible only
// when a PUT, a CompleteMultipartUpload or a copy finishes. Concurrent writers
// of the same content converge on identical bytes under one key.
//
// Abandoned temp objects and multipart uploads (crashed nodes) are removed by
// CleanTemp. The credentials need s3:GetObject, s3:PutObject,
// s3:DeleteObject, s3:ListBucket, s3:ListBucketMultipartUploads and
// s3:AbortMultipartUpload on the bucket. Without s3:ListBucket, S3 answers
// 403 instead of 404 for missing keys and Stat cannot report ErrNotFound.
package s3

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/ismoilovdevml/hangar/internal/core"
)

// Kind is the blob_stores.kind value of this driver.
const Kind = "s3"

// Part size bounds. S3 rejects multipart parts smaller than 5 MiB (except the
// last) and allows at most 10,000 parts, so the part size caps the largest
// blob: 16 MiB parts allow blobs up to about 156 GiB.
const (
	MinPartSize     = 5 << 20
	MaxPartSize     = 5 << 30
	DefaultPartSize = 16 << 20
	maxParts        = 10000
)

// DefaultRegion is used when Config.Region is empty.
const DefaultRegion = "us-east-1"

const contentType = "application/octet-stream"

// Config configures an S3 blob store. The JSON keys are the keys accepted by
// Open (blob_stores.config) and match the YAML keys under storage.s3.
type Config struct {
	// Endpoint is the S3 endpoint as a URL ("http://minio:9000",
	// "https://s3.eu-central-1.amazonaws.com") or a bare host[:port], which
	// implies https. Empty means AWS S3 (s3.amazonaws.com).
	Endpoint string `json:"endpoint" yaml:"endpoint"`
	// Region is the bucket's region; default us-east-1. Setting it avoids a
	// bucket-location lookup on first use.
	Region string `json:"region" yaml:"region"`
	// Bucket must exist; Hangar never creates it.
	Bucket string `json:"bucket" yaml:"bucket"`
	// Prefix namespaces every key, for sharing a bucket ("hangar/").
	Prefix string `json:"prefix" yaml:"prefix"`
	// AccessKeyID and SecretAccessKey select static credentials. When both are
	// empty the standard chain is used: AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY
	// (+ AWS_SESSION_TOKEN), MINIO_ROOT_USER/MINIO_ROOT_PASSWORD, the shared
	// credentials file (~/.aws/credentials, AWS_PROFILE), then IAM: web
	// identity (AWS_WEB_IDENTITY_TOKEN_FILE, EKS IRSA), ECS container
	// credentials and the EC2 instance metadata service.
	AccessKeyID     string `json:"access_key_id" yaml:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key" yaml:"secret_access_key"`
	// SessionToken accompanies temporary static credentials.
	SessionToken string `json:"session_token" yaml:"session_token"`
	// UsePathStyle addresses buckets as https://host/bucket/key (MinIO and
	// most self-hosted services). False uses virtual-hosted style,
	// https://bucket.host/key, as AWS prefers.
	UsePathStyle bool `json:"use_path_style" yaml:"use_path_style"`
	// CAFile is a PEM bundle trusted in addition to the system roots, for
	// endpoints behind a private CA.
	CAFile string `json:"ca_file" yaml:"ca_file"`
	// PartSize is the multipart part size in bytes, and the most memory one
	// upload holds. Default 16 MiB; between 5 MiB and 5 GiB.
	PartSize int64 `json:"part_size" yaml:"part_size"`

	// uploadLimits overrides the client's S3 limits (tests only).
	uploadLimits *minio.UploadLimits
}

// Store is an S3 blob store. It is safe for concurrent use.
type Store struct {
	core     *minio.Core
	bucket   string
	prefix   string
	partSize int64
	// signPayload is set for plain-HTTP endpoints, where request bodies are
	// signed with their sha256 instead of relying on TLS for integrity.
	signPayload bool
}

var _ core.BlobStore = (*Store)(nil)

// Open builds a store from a decoded blob_stores.config document (or any
// map with the JSON keys of Config). It is the constructor the storage
// registry uses for kind "s3":
//
//	store, err := s3.Open(ctx, map[string]any{"bucket": "hangar", "endpoint": "http://minio:9000", "use_path_style": true})
//
// Unknown keys are rejected so typos do not silently fall back to defaults.
// Open does not contact the endpoint; call Ping for that.
func Open(ctx context.Context, cfg map[string]any) (core.BlobStore, error) {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("s3 blob store config: %w: %w", err, core.ErrInvalid)
	}
	var c Config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("s3 blob store config: %w: %w", err, core.ErrInvalid)
	}
	return New(ctx, c)
}

// New builds a store from cfg. It validates the configuration and prepares
// the client but does not contact the endpoint; call Ping for that.
func New(_ context.Context, cfg Config) (*Store, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("s3 blob store: bucket is required: %w", core.ErrInvalid)
	}
	partSize := cfg.PartSize
	if partSize == 0 {
		partSize = DefaultPartSize
	}
	if partSize < MinPartSize || partSize > MaxPartSize {
		return nil, fmt.Errorf("s3 blob store: part_size %d must be between %d and %d: %w",
			partSize, MinPartSize, MaxPartSize, core.ErrInvalid)
	}
	host, secure, err := parseEndpoint(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	if (cfg.AccessKeyID == "") != (cfg.SecretAccessKey == "") {
		return nil, fmt.Errorf("s3 blob store: access_key_id and secret_access_key must be set together: %w", core.ErrInvalid)
	}
	tr, err := transport(secure, cfg.CAFile)
	if err != nil {
		return nil, err
	}
	var creds *credentials.Credentials
	if cfg.AccessKeyID != "" {
		creds = credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, cfg.SessionToken)
	} else {
		creds = credentials.NewChainCredentials([]credentials.Provider{
			&credentials.EnvAWS{},
			&credentials.EnvMinio{},
			&credentials.FileAWSCredentials{},
			&credentials.IAM{Client: &http.Client{Transport: tr}},
		})
	}
	region := cfg.Region
	if region == "" {
		region = DefaultRegion
	}
	lookup := minio.BucketLookupDNS
	if cfg.UsePathStyle {
		lookup = minio.BucketLookupPath
	}
	client, err := minio.New(host, &minio.Options{
		Creds:        creds,
		Secure:       secure,
		Transport:    tr,
		Region:       region,
		BucketLookup: lookup,
		UploadLimits: cfg.uploadLimits,
	})
	if err != nil {
		return nil, fmt.Errorf("s3 blob store: %w: %w", err, core.ErrInvalid)
	}
	return &Store{
		core:        &minio.Core{Client: client},
		bucket:      cfg.Bucket,
		prefix:      normalizePrefix(cfg.Prefix),
		partSize:    partSize,
		signPayload: !secure,
	}, nil
}

// parseEndpoint returns host[:port] and whether TLS is used.
func parseEndpoint(endpoint string) (string, bool, error) {
	if endpoint == "" {
		return "s3.amazonaws.com", true, nil
	}
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
		return "", false, fmt.Errorf("s3 blob store: endpoint %q must be a scheme://host[:port] URL without path or credentials: %w",
			endpoint, core.ErrInvalid)
	}
	switch u.Scheme {
	case "https":
		return u.Host, true, nil
	case "http":
		return u.Host, false, nil
	default:
		return "", false, fmt.Errorf("s3 blob store: endpoint scheme %q: want http or https: %w", u.Scheme, core.ErrInvalid)
	}
}

// transport is the minio default transport (environment proxy, timeouts,
// no compression) plus the extra CA bundle.
func transport(secure bool, caFile string) (*http.Transport, error) {
	tr, err := minio.DefaultTransport(secure)
	if err != nil {
		return nil, fmt.Errorf("s3 blob store: %w", err)
	}
	if caFile == "" {
		return tr, nil
	}
	pem, err := os.ReadFile(caFile) //nolint:gosec // operator-supplied CA path
	if err != nil {
		return nil, fmt.Errorf("s3 blob store: ca_file: %w", err)
	}
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if !roots.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("s3 blob store: ca_file %s: no PEM certificates found: %w", caFile, core.ErrInvalid)
	}
	if tr.TLSClientConfig == nil {
		tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	tr.TLSClientConfig.RootCAs = roots
	return tr, nil
}

func normalizePrefix(p string) string {
	p = strings.Trim(p, "/")
	if p == "" {
		return ""
	}
	return p + "/"
}

// Bucket returns the bucket name.
func (s *Store) Bucket() string { return s.bucket }

// Prefix returns the normalized key prefix ("" or ending in "/").
func (s *Store) Prefix() string { return s.prefix }

// BlobKey returns the object key of digest d, or ErrInvalid.
func (s *Store) BlobKey(d core.Digest) (string, error) {
	if err := d.Validate(); err != nil {
		return "", err
	}
	if d.Algorithm() != core.AlgorithmSHA256 {
		return "", fmt.Errorf("digest %s: s3 store addresses sha256 only: %w", d, core.ErrInvalid)
	}
	return s.blobKey(d.Hex()), nil
}

func (s *Store) blobKey(h string) string {
	return s.prefix + "blobs/" + core.AlgorithmSHA256 + "/" + h[:2] + "/" + h[2:4] + "/" + h
}

func (s *Store) blobsPrefix() string { return s.prefix + "blobs/" + core.AlgorithmSHA256 + "/" }

func (s *Store) tmpPrefix() string { return s.prefix + "tmp/" }

func (s *Store) newTmpKey() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return s.tmpPrefix() + strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + hex.EncodeToString(b[:])
}

// Put implements core.BlobStore.
func (s *Store) Put(ctx context.Context, r io.Reader) (core.Digest, int64, error) {
	h := sha256.New()
	src := io.TeeReader(ctxReader{ctx: ctx, r: r}, h)
	part, eof, err := fillPart(src, nil, s.partSize)
	if err != nil {
		return "", 0, fmt.Errorf("s3 put: %w", err)
	}
	if eof {
		d := core.NewSHA256Digest(h.Sum(nil))
		if err := s.putSingle(ctx, d, part); err != nil {
			return "", 0, err
		}
		return d, int64(len(part)), nil
	}
	return s.putMultipart(ctx, src, h, part)
}

// putSingle writes content whose digest is already known to its final key.
func (s *Store) putSingle(ctx context.Context, d core.Digest, data []byte) error {
	key := s.blobKey(d.Hex())
	if exists, err := s.exists(ctx, key); err != nil {
		return fmt.Errorf("s3 put %s: %w", d, err)
	} else if exists {
		return nil // deduplicated
	}
	if _, err := s.core.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)), "",
		s.payloadSHA256(data), minio.PutObjectOptions{ContentType: contentType, DisableContentSha256: true}); err != nil {
		return fmt.Errorf("s3 put %s: %w", d, err)
	}
	return nil
}

// putMultipart streams the rest of src as a multipart upload to a temp key,
// starting with the already-read first part, then commits it.
func (s *Store) putMultipart(ctx context.Context, src io.Reader, h hash.Hash, first []byte) (_ core.Digest, _ int64, err error) {
	tmpKey := s.newTmpKey()
	uploadID, err := s.core.NewMultipartUpload(ctx, s.bucket, tmpKey, minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return "", 0, fmt.Errorf("s3 put: start upload: %w", err)
	}
	completed := false
	defer func() {
		if !completed {
			cctx, cancel := cleanupContext(ctx)
			defer cancel()
			_ = s.core.AbortMultipartUpload(cctx, s.bucket, tmpKey, uploadID)
		}
	}()

	var parts []minio.CompletePart
	var total int64
	part, eof := first, false
	for num := 1; ; num++ {
		if num > maxParts {
			return "", 0, fmt.Errorf("s3 put: blob exceeds %d parts of %d bytes; raise part_size: %w",
				maxParts, s.partSize, core.ErrInvalid)
		}
		p, err := s.core.PutObjectPart(ctx, s.bucket, tmpKey, uploadID, num, bytes.NewReader(part), int64(len(part)),
			minio.PutObjectPartOptions{Sha256Hex: s.payloadSHA256(part), DisableContentSha256: true})
		if err != nil {
			return "", 0, fmt.Errorf("s3 put: upload part %d: %w", num, err)
		}
		parts = append(parts, minio.CompletePart{PartNumber: num, ETag: p.ETag})
		total += int64(len(part))
		if eof {
			break
		}
		// The part buffer is reused: PutObjectPart has finished reading it.
		if part, eof, err = fillPart(src, part, s.partSize); err != nil {
			return "", 0, fmt.Errorf("s3 put: %w", err)
		}
		if len(part) == 0 {
			break
		}
	}
	if _, err := s.core.CompleteMultipartUpload(ctx, s.bucket, tmpKey, uploadID, parts,
		minio.PutObjectOptions{ContentType: contentType}); err != nil {
		return "", 0, fmt.Errorf("s3 put: complete upload: %w", err)
	}
	completed = true
	defer func() {
		cctx, cancel := cleanupContext(ctx)
		defer cancel()
		_ = s.core.RemoveObject(cctx, s.bucket, tmpKey, minio.RemoveObjectOptions{})
	}()

	d := core.NewSHA256Digest(h.Sum(nil))
	key := s.blobKey(d.Hex())
	if exists, err := s.exists(ctx, key); err != nil {
		return "", 0, fmt.Errorf("s3 put %s: %w", d, err)
	} else if exists {
		return d, total, nil // deduplicated; the deferred remove drops the temp object
	}
	// Server-side copy: one CopyObject up to the single-copy limit (5 GiB on
	// S3), a multipart UploadPartCopy above it. No bytes pass through Hangar.
	dst := minio.CopyDestOptions{Bucket: s.bucket, Object: key}
	from := minio.CopySrcOptions{Bucket: s.bucket, Object: tmpKey}
	if total <= s.core.Client.UploadLimits().MaxPartSize {
		_, err = s.core.Client.CopyObject(ctx, dst, from)
	} else {
		_, err = s.core.ComposeObject(ctx, dst, from)
	}
	if err != nil {
		return "", 0, fmt.Errorf("s3 put %s: commit: %w", d, err)
	}
	return d, total, nil
}

// payloadSHA256 signs request bodies on plain-HTTP endpoints. Over TLS the
// payload is sent unsigned, as AWS SDKs do.
func (s *Store) payloadSHA256(b []byte) string {
	if !s.signPayload {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (s *Store) exists(ctx context.Context, key string) (bool, error) {
	_, err := s.core.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, err
}

// Get implements core.BlobStore. The returned reader fetches content lazily
// with ranged GETs, so Seek (used by net/http for Range requests) costs
// nothing until the next Read.
func (s *Store) Get(ctx context.Context, d core.Digest) (io.ReadSeekCloser, error) {
	key, err := s.BlobKey(d)
	if err != nil {
		return nil, err
	}
	info, err := s.core.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return nil, s.mapErr("get", d, err)
	}
	return &objectReader{ctx: ctx, core: s.core, bucket: s.bucket, key: key, size: info.Size, etag: info.ETag}, nil
}

// Stat implements core.BlobStore. CreatedAt is the object's Last-Modified time.
func (s *Store) Stat(ctx context.Context, d core.Digest) (core.BlobInfo, error) {
	key, err := s.BlobKey(d)
	if err != nil {
		return core.BlobInfo{}, err
	}
	info, err := s.core.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return core.BlobInfo{}, s.mapErr("stat", d, err)
	}
	return core.BlobInfo{Digest: d, Size: info.Size, CreatedAt: info.LastModified.UTC()}, nil
}

// Delete implements core.BlobStore. S3 deletes are idempotent, so the blob
// is checked first to report ErrNotFound as the contract requires.
func (s *Store) Delete(ctx context.Context, d core.Digest) error {
	key, err := s.BlobKey(d)
	if err != nil {
		return err
	}
	if _, err := s.core.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{}); err != nil {
		return s.mapErr("delete", d, err)
	}
	if err := s.core.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return s.mapErr("delete", d, err)
	}
	return nil
}

// Ping checks that the bucket is reachable with the configured credentials.
// Used by /readyz.
func (s *Store) Ping(ctx context.Context) error {
	ok, err := s.core.BucketExists(ctx, s.bucket)
	if err != nil {
		return fmt.Errorf("s3 blob store: bucket %q: %w", s.bucket, err)
	}
	if !ok {
		return fmt.Errorf("s3 blob store: bucket %q does not exist", s.bucket)
	}
	return nil
}

// CleanTemp removes temp objects and aborts multipart uploads under tmp/
// that are older than maxAge (left by crashed uploads). It returns how many
// were removed. Uploads in progress are younger than any sensible maxAge
// (use at least a few hours).
func (s *Store) CleanTemp(ctx context.Context, maxAge time.Duration) (int, error) {
	cutoff := time.Now().Add(-maxAge)
	removed := 0
	var errs []error
	lctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for obj := range s.core.Client.ListObjects(lctx, s.bucket, minio.ListObjectsOptions{Prefix: s.tmpPrefix(), Recursive: true}) {
		if obj.Err != nil {
			errs = append(errs, obj.Err)
			break
		}
		if obj.LastModified.After(cutoff) {
			continue
		}
		if err := s.core.RemoveObject(ctx, s.bucket, obj.Key, minio.RemoveObjectOptions{}); err != nil {
			errs = append(errs, err)
			continue
		}
		removed++
	}

	keyMarker, uploadMarker := "", ""
	for {
		res, err := s.core.ListMultipartUploads(ctx, s.bucket, s.tmpPrefix(), keyMarker, uploadMarker, "", 1000)
		if err != nil {
			errs = append(errs, err)
			break
		}
		for _, u := range res.Uploads {
			if u.Initiated.After(cutoff) {
				continue
			}
			if err := s.core.AbortMultipartUpload(ctx, s.bucket, u.Key, u.UploadID); err != nil && !isNotFound(err) {
				errs = append(errs, err)
				continue
			}
			removed++
		}
		if !res.IsTruncated || (res.NextKeyMarker == "" && res.NextUploadIDMarker == "") {
			break
		}
		keyMarker, uploadMarker = res.NextKeyMarker, res.NextUploadIDMarker
	}
	if len(errs) > 0 {
		return removed, fmt.Errorf("s3 clean temp: %w", errors.Join(errs...))
	}
	return removed, nil
}

// Walk calls fn for every committed blob, in key order. Keys under the blob
// prefix that are not valid blob keys are skipped. Returning an error from
// fn stops the walk and returns that error. Used by the orphan scan.
func (s *Store) Walk(ctx context.Context, fn func(core.BlobInfo) error) error {
	lctx, cancel := context.WithCancel(ctx)
	defer cancel()
	base := s.blobsPrefix()
	for obj := range s.core.Client.ListObjects(lctx, s.bucket, minio.ListObjectsOptions{Prefix: base, Recursive: true}) {
		if obj.Err != nil {
			return fmt.Errorf("s3 walk: %w", obj.Err)
		}
		d, ok := s.digestFromKey(obj.Key)
		if !ok {
			continue
		}
		if err := fn(core.BlobInfo{Digest: d, Size: obj.Size, CreatedAt: obj.LastModified.UTC()}); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (s *Store) digestFromKey(key string) (core.Digest, bool) {
	rest, ok := strings.CutPrefix(key, s.blobsPrefix())
	if !ok {
		return "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		return "", false
	}
	d := core.Digest(core.AlgorithmSHA256 + ":" + parts[2])
	if d.Validate() != nil || parts[0] != parts[2][:2] || parts[1] != parts[2][2:4] {
		return "", false
	}
	return d, true
}

func (s *Store) mapErr(op string, d core.Digest, err error) error {
	if isNotFound(err) {
		return fmt.Errorf("blob %s: %w", d, core.ErrNotFound)
	}
	return fmt.Errorf("s3 %s %s: %w", op, d, err)
}

func isNotFound(err error) bool {
	resp := minio.ToErrorResponse(err)
	switch resp.Code {
	case minio.NoSuchKey, minio.NoSuchUpload:
		return true
	case minio.NoSuchBucket:
		return false
	}
	return resp.StatusCode == http.StatusNotFound
}

// fillPart reads from src into buf (reused, grown up to limit) until it holds
// limit bytes or src is exhausted. eof reports that src has no more data.
// The buffer grows by doubling, so small blobs use little memory.
func fillPart(src io.Reader, buf []byte, limit int64) (_ []byte, eof bool, _ error) {
	buf = buf[:0]
	for int64(len(buf)) < limit {
		if len(buf) == cap(buf) {
			next := max(int64(cap(buf))*2, 32<<10)
			next = min(next, limit)
			grown := make([]byte, len(buf), next)
			copy(grown, buf)
			buf = grown
		}
		n, err := src.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if errors.Is(err, io.EOF) {
			return buf, true, nil
		}
		if err != nil {
			return buf, false, err
		}
	}
	return buf, false, nil
}

// cleanupContext outlives a cancelled request so aborts and temp deletes
// still run, but bounds them in time.
func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
}

// ctxReader aborts a copy when ctx is cancelled (client went away).
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
