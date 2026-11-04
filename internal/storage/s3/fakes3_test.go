package s3

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// fakeS3 is an in-memory, path-style S3 endpoint implementing the subset of
// the S3 REST API the store uses: HeadBucket, GetBucketLocation, PutObject,
// CopyObject, GetObject (Range, If-Match), HeadObject, DeleteObject,
// ListObjectsV2, CreateMultipartUpload, UploadPart, UploadPartCopy,
// CompleteMultipartUpload, AbortMultipartUpload and ListMultipartUploads.
// Signatures are not verified. Streaming (aws-chunked) bodies are rejected so
// tests catch a client that would send them.
type fakeS3 struct {
	mu      sync.Mutex
	buckets map[string]map[string]*fakeObject
	uploads map[string]*fakeUpload
	// minPart is the smallest non-final multipart part accepted.
	minPart int
	// fail, if set, may answer a request with an injected error status.
	fail func(r *http.Request) (status int, code string)
	// ops counts requests by operation name.
	ops map[string]int
}

type fakeObject struct {
	data     []byte
	etag     string
	modified time.Time
}

type fakeUpload struct {
	bucket, key string
	parts       map[int][]byte
	initiated   time.Time
}

func newFakeS3(buckets ...string) *fakeS3 {
	f := &fakeS3{
		buckets: map[string]map[string]*fakeObject{},
		uploads: map[string]*fakeUpload{},
		minPart: MinPartSize,
		ops:     map[string]int{},
	}
	for _, b := range buckets {
		f.buckets[b] = map[string]*fakeObject{}
	}
	return f
}

// count returns how many requests of operation op were served.
func (f *fakeS3) count(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ops[op]
}

// keys returns the object keys of bucket with the given prefix, sorted.
func (f *fakeS3) keys(bucket, prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for k := range f.buckets[bucket] {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// uploadCount returns the number of incomplete multipart uploads.
func (f *fakeS3) uploadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.uploads)
}

// age moves every object and upload under prefix back in time by d.
func (f *fakeS3) age(bucket, prefix string, d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, o := range f.buckets[bucket] {
		if strings.HasPrefix(k, prefix) {
			o.modified = o.modified.Add(-d)
		}
	}
	for _, u := range f.uploads {
		if u.bucket == bucket && strings.HasPrefix(u.key, prefix) {
			u.initiated = u.initiated.Add(-d)
		}
	}
}

func md5ETag(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

func randomID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	q := r.URL.Query()
	op := f.operation(r.Method, key, q, r.Header)

	f.mu.Lock()
	f.ops[op]++
	fail := f.fail
	f.mu.Unlock()
	if fail != nil {
		if status, code := fail(r); status != 0 {
			writeError(w, r, status, code, "injected failure")
			return
		}
	}
	if strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
		writeError(w, r, http.StatusNotImplemented, "NotImplemented", "streaming signatures are not supported by this fake")
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	objs, ok := f.buckets[bucket]
	if !ok {
		writeError(w, r, http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist")
		return
	}
	switch op {
	case "HeadBucket":
		w.WriteHeader(http.StatusOK)
	case "GetBucketLocation":
		writeXML(w, http.StatusOK, struct {
			XMLName xml.Name `xml:"LocationConstraint"`
			Value   string   `xml:",chardata"`
		}{Value: DefaultRegion})
	case "ListObjectsV2":
		f.listObjects(w, objs, q)
	case "ListMultipartUploads":
		f.listUploads(w, bucket, q)
	case "PutObject":
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "IncompleteBody", err.Error())
			return
		}
		if cl := r.Header.Get("Content-Length"); cl != "" && cl != strconv.Itoa(len(body)) {
			writeError(w, r, http.StatusBadRequest, "IncompleteBody", "length mismatch")
			return
		}
		o := &fakeObject{data: body, etag: md5ETag(body), modified: time.Now().UTC()}
		objs[key] = o
		w.Header().Set("ETag", `"`+o.etag+`"`)
		w.WriteHeader(http.StatusOK)
	case "CopyObject":
		src, ok := f.copySource(r)
		if !ok {
			writeError(w, r, http.StatusNotFound, "NoSuchKey", "copy source not found")
			return
		}
		o := &fakeObject{data: bytes.Clone(src.data), etag: src.etag, modified: time.Now().UTC()}
		objs[key] = o
		writeXML(w, http.StatusOK, struct {
			XMLName      xml.Name `xml:"CopyObjectResult"`
			LastModified string
			ETag         string
		}{LastModified: o.modified.Format(time.RFC3339Nano), ETag: `"` + o.etag + `"`})
	case "GetObject", "HeadObject":
		f.getObject(w, r, objs, key, op == "HeadObject")
	case "DeleteObject":
		delete(objs, key)
		w.WriteHeader(http.StatusNoContent)
	case "CreateMultipartUpload":
		id := randomID()
		f.uploads[id] = &fakeUpload{bucket: bucket, key: key, parts: map[int][]byte{}, initiated: time.Now().UTC()}
		writeXML(w, http.StatusOK, struct {
			XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
			Bucket   string
			Key      string
			UploadID string `xml:"UploadId"`
		}{Bucket: bucket, Key: key, UploadID: id})
	case "UploadPart", "UploadPartCopy":
		f.uploadPart(w, r, bucket, key, q, op == "UploadPartCopy")
	case "CompleteMultipartUpload":
		f.completeUpload(w, r, objs, bucket, key, q.Get("uploadId"))
	case "AbortMultipartUpload":
		u, ok := f.uploads[q.Get("uploadId")]
		if !ok || u.bucket != bucket || u.key != key {
			writeError(w, r, http.StatusNotFound, "NoSuchUpload", "no such upload")
			return
		}
		delete(f.uploads, q.Get("uploadId"))
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, r, http.StatusNotImplemented, "NotImplemented", op)
	}
}

func (f *fakeS3) operation(method, key string, q url.Values, h http.Header) string {
	copySrc := h.Get("X-Amz-Copy-Source") != ""
	if key == "" {
		switch {
		case method == http.MethodHead:
			return "HeadBucket"
		case method == http.MethodGet && q.Has("location"):
			return "GetBucketLocation"
		case method == http.MethodGet && q.Has("uploads"):
			return "ListMultipartUploads"
		case method == http.MethodGet && q.Get("list-type") == "2":
			return "ListObjectsV2"
		}
		return method + " bucket"
	}
	switch method {
	case http.MethodPut:
		switch {
		case q.Has("uploadId") && copySrc:
			return "UploadPartCopy"
		case q.Has("uploadId"):
			return "UploadPart"
		case copySrc:
			return "CopyObject"
		}
		return "PutObject"
	case http.MethodPost:
		if q.Has("uploads") {
			return "CreateMultipartUpload"
		}
		if q.Has("uploadId") {
			return "CompleteMultipartUpload"
		}
	case http.MethodDelete:
		if q.Has("uploadId") {
			return "AbortMultipartUpload"
		}
		return "DeleteObject"
	case http.MethodGet:
		return "GetObject"
	case http.MethodHead:
		return "HeadObject"
	}
	return method + " object"
}

func (f *fakeS3) copySource(r *http.Request) (*fakeObject, bool) {
	src, err := url.PathUnescape(r.Header.Get("X-Amz-Copy-Source"))
	if err != nil {
		return nil, false
	}
	src, _, _ = strings.Cut(src, "?") // versionId
	b, k, _ := strings.Cut(strings.TrimPrefix(src, "/"), "/")
	o, ok := f.buckets[b][k]
	return o, ok
}

func (f *fakeS3) getObject(w http.ResponseWriter, r *http.Request, objs map[string]*fakeObject, key string, head bool) {
	o, ok := objs[key]
	if !ok {
		writeError(w, r, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
		return
	}
	if m := r.Header.Get("If-Match"); m != "" && strings.Trim(m, `"`) != o.etag {
		writeError(w, r, http.StatusPreconditionFailed, "PreconditionFailed", "If-Match failed")
		return
	}
	size := int64(len(o.data))
	start, end := int64(0), size-1
	status := http.StatusOK
	if rg := r.Header.Get("Range"); rg != "" && !head {
		var ok bool
		start, end, ok = parseRange(rg, size)
		if !ok {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
			writeError(w, r, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", "The requested range is not satisfiable")
			return
		}
		status = http.StatusPartialContent
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
	}
	w.Header().Set("ETag", `"`+o.etag+`"`)
	w.Header().Set("Last-Modified", o.modified.Format(http.TimeFormat))
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Accept-Ranges", "bytes")
	if head {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(status)
	if size > 0 {
		_, _ = w.Write(o.data[start : end+1])
	}
}

// parseRange handles a single "bytes=a-b", "bytes=a-" or "bytes=-n" range.
func parseRange(h string, size int64) (start, end int64, ok bool) {
	spec, found := strings.CutPrefix(h, "bytes=")
	if !found || strings.Contains(spec, ",") {
		return 0, 0, false
	}
	a, b, _ := strings.Cut(spec, "-")
	if a == "" {
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		start, end = max(size-n, 0), size-1
	} else {
		var err error
		if start, err = strconv.ParseInt(a, 10, 64); err != nil {
			return 0, 0, false
		}
		end = size - 1
		if b != "" {
			if end, err = strconv.ParseInt(b, 10, 64); err != nil {
				return 0, 0, false
			}
			end = min(end, size-1)
		}
	}
	if start >= size || start > end {
		return 0, 0, false
	}
	return start, end, true
}

func (f *fakeS3) uploadPart(w http.ResponseWriter, r *http.Request, bucket, key string, q url.Values, isCopy bool) {
	u, ok := f.uploads[q.Get("uploadId")]
	if !ok || u.bucket != bucket || u.key != key {
		writeError(w, r, http.StatusNotFound, "NoSuchUpload", "no such upload")
		return
	}
	num, err := strconv.Atoi(q.Get("partNumber"))
	if err != nil || num < 1 || num > maxParts {
		writeError(w, r, http.StatusBadRequest, "InvalidArgument", "bad part number")
		return
	}
	var data []byte
	if isCopy {
		src, ok := f.copySource(r)
		if !ok {
			writeError(w, r, http.StatusNotFound, "NoSuchKey", "copy source not found")
			return
		}
		data = src.data
		if rg := r.Header.Get("X-Amz-Copy-Source-Range"); rg != "" {
			start, end, ok := parseRange(rg, int64(len(src.data)))
			if !ok {
				writeError(w, r, http.StatusBadRequest, "InvalidRange", "bad copy range")
				return
			}
			data = src.data[start : end+1]
		}
		data = bytes.Clone(data)
	} else if data, err = io.ReadAll(r.Body); err != nil {
		writeError(w, r, http.StatusBadRequest, "IncompleteBody", err.Error())
		return
	}
	u.parts[num] = data
	etag := `"` + md5ETag(data) + `"`
	if isCopy {
		writeXML(w, http.StatusOK, struct {
			XMLName      xml.Name `xml:"CopyPartResult"`
			LastModified string
			ETag         string
		}{LastModified: time.Now().UTC().Format(time.RFC3339Nano), ETag: etag})
		return
	}
	w.Header().Set("ETag", etag)
	w.WriteHeader(http.StatusOK)
}

func (f *fakeS3) completeUpload(w http.ResponseWriter, r *http.Request, objs map[string]*fakeObject, bucket, key, id string) {
	u, ok := f.uploads[id]
	if !ok || u.bucket != bucket || u.key != key {
		writeError(w, r, http.StatusNotFound, "NoSuchUpload", "no such upload")
		return
	}
	var req struct {
		Parts []struct {
			PartNumber int
			ETag       string
		} `xml:"Part"`
	}
	if err := xml.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Parts) == 0 {
		writeError(w, r, http.StatusBadRequest, "MalformedXML", "bad complete request")
		return
	}
	var all bytes.Buffer
	var sums []byte
	prev := 0
	for i, p := range req.Parts {
		data, ok := u.parts[p.PartNumber]
		if !ok || p.PartNumber <= prev || strings.Trim(p.ETag, `"`) != md5ETag(data) {
			writeError(w, r, http.StatusBadRequest, "InvalidPart", fmt.Sprintf("part %d", p.PartNumber))
			return
		}
		if i < len(req.Parts)-1 && len(data) < f.minPart {
			writeError(w, r, http.StatusBadRequest, "EntityTooSmall", fmt.Sprintf("part %d", p.PartNumber))
			return
		}
		prev = p.PartNumber
		all.Write(data)
		sum := md5.Sum(data)
		sums = append(sums, sum[:]...)
	}
	etag := fmt.Sprintf("%s-%d", md5ETag(sums), len(req.Parts))
	objs[key] = &fakeObject{data: all.Bytes(), etag: etag, modified: time.Now().UTC()}
	delete(f.uploads, id)
	writeXML(w, http.StatusOK, struct {
		XMLName xml.Name `xml:"CompleteMultipartUploadResult"`
		Bucket  string
		Key     string
		ETag    string
	}{Bucket: bucket, Key: key, ETag: `"` + etag + `"`})
}

type xmlContents struct {
	Key          string
	LastModified string
	ETag         string
	Size         int64
	StorageClass string
}

func (f *fakeS3) listObjects(w http.ResponseWriter, objs map[string]*fakeObject, q url.Values) {
	prefix := q.Get("prefix")
	after := q.Get("start-after")
	if t := q.Get("continuation-token"); t != "" {
		after = t
	}
	maxKeys := 1000
	if mk, err := strconv.Atoi(q.Get("max-keys")); err == nil && mk > 0 && mk < maxKeys {
		maxKeys = mk
	}
	var keys []string
	for k := range objs {
		if strings.HasPrefix(k, prefix) && k > after {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	truncated := len(keys) > maxKeys
	if truncated {
		keys = keys[:maxKeys]
	}
	enc := func(s string) string {
		if q.Get("encoding-type") == "url" {
			return url.QueryEscape(s)
		}
		return s
	}
	res := struct {
		XMLName               xml.Name `xml:"ListBucketResult"`
		Name                  string
		Prefix                string
		KeyCount              int
		MaxKeys               int
		IsTruncated           bool
		NextContinuationToken string `xml:",omitempty"`
		EncodingType          string `xml:",omitempty"`
		Contents              []xmlContents
	}{Prefix: enc(prefix), KeyCount: len(keys), MaxKeys: maxKeys, IsTruncated: truncated, EncodingType: q.Get("encoding-type")}
	for _, k := range keys {
		o := objs[k]
		res.Contents = append(res.Contents, xmlContents{
			Key: enc(k), LastModified: o.modified.Format(time.RFC3339Nano), ETag: `"` + o.etag + `"`,
			Size: int64(len(o.data)), StorageClass: "STANDARD",
		})
	}
	if truncated {
		res.NextContinuationToken = keys[len(keys)-1]
	}
	writeXML(w, http.StatusOK, res)
}

func (f *fakeS3) listUploads(w http.ResponseWriter, bucket string, q url.Values) {
	type xmlUpload struct {
		Key       string
		UploadID  string `xml:"UploadId"`
		Initiated string
	}
	res := struct {
		XMLName     xml.Name `xml:"ListMultipartUploadsResult"`
		Bucket      string
		Prefix      string
		MaxUploads  int
		IsTruncated bool
		Uploads     []xmlUpload `xml:"Upload"`
	}{Bucket: bucket, Prefix: q.Get("prefix"), MaxUploads: 1000}
	for id, u := range f.uploads {
		if u.bucket == bucket && strings.HasPrefix(u.key, q.Get("prefix")) {
			res.Uploads = append(res.Uploads, xmlUpload{Key: u.key, UploadID: id, Initiated: u.initiated.Format(time.RFC3339Nano)})
		}
	}
	sort.Slice(res.Uploads, func(i, j int) bool { return res.Uploads[i].Key < res.Uploads[j].Key })
	writeXML(w, http.StatusOK, res)
}

func writeXML(w http.ResponseWriter, status int, v any) {
	b, err := xml.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Set("Content-Length", strconv.Itoa(len(xml.Header)+len(b)))
	w.WriteHeader(status)
	_, _ = io.WriteString(w, xml.Header)
	_, _ = w.Write(b)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	if r.Method == http.MethodHead {
		w.WriteHeader(status)
		return
	}
	writeXML(w, status, struct {
		XMLName   xml.Name `xml:"Error"`
		Code      string
		Message   string
		Resource  string
		RequestID string `xml:"RequestId"`
	}{Code: code, Message: msg, Resource: r.URL.Path, RequestID: randomID()})
}
