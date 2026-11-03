# 0003. Content-addressed, deduplicated blob storage

Date: 2026-11-03 · Status: Accepted

## Context
The same bytes arrive many times: one layer in many images, one jar in hosted
and proxy repositories, re-uploads. Storage cost and integrity matter more
than anything else in a registry.

## Decision
- Every blob is addressed by its sha256 digest (`sha256:<hex>`, OCI syntax) and
  stored once per blob store. Assets reference blobs by digest; many assets may
  share one blob.
- `core.BlobStore` is the only storage interface: `Put` streams while hashing
  and commits atomically, `Get` returns an `io.ReadSeekCloser` (Range requests),
  `Stat`, and `Delete` (garbage collection only). Bytes are never buffered whole
  in memory.
- Drivers: filesystem (`internal/storage/fs`: temp file, fsync, atomic rename,
  `blobs/sha256/ab/<hex>` layout) and S3-compatible object storage.
- Lifecycle: the `blobs` table keeps a reference count per store; when it drops
  to zero the blob is soft-deleted, and compaction deletes the bytes after a
  grace period. Unreferenced blobs left by aborted uploads are reclaimed the
  same way.
- Every driver must pass the shared conformance suite
  `coretest.RunBlobStoreTests`.

## Consequences
- Dedup and integrity verification are free: the address is the checksum.
- Moving repositories between stores is a copy of referenced digests.
- Deletion is two-phase; "delete" in the UI frees space only after compaction,
  which the Blob stores screen reports as "reclaimable".
