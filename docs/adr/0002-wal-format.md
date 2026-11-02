# ADR 0002: WAL record format with CRC-32C and dense LSNs

## Status

Accepted.

## Context

Every acknowledged write must survive a crash. The log is the only copy of
writes made since the last snapshot, so recovery has to tell a complete
record from one that was being written when the process died, and from one
that was damaged on disk later.

## Decision

Each record is framed as:

```
+-----------+-----------+---------------------------------+
| len (u32) | crc (u32) | payload: lsn (u64) || data      |
+-----------+-----------+---------------------------------+
```

- Little endian. `len` is the payload length; `crc` is CRC-32C
  (Castagnoli) of the payload, which has hardware support on amd64 and arm64
  through `hash/crc32`.
- The LSN is inside the checksummed payload, and LSNs are dense: record
  `n + 1` must follow record `n`. A stale record left after a truncation, or
  a segment from a different history, fails this check even if its CRC is
  valid.
- `len` is bounded (256 MiB). A corrupted length cannot trigger a huge
  allocation.
- A frame is written with one `write(2)` on an `O_APPEND` file. If the call
  fails part way, the file is truncated back to the previous size.
- Segments are named after their first LSN (`wal-<20 digits>.log`) and
  rotated at 64 MiB and at every snapshot. File names alone tell recovery
  which files a snapshot makes obsolete and whether one is missing.
- The data inside a record is opaque to the WAL. The collection layer
  encodes one record per request batch (`op | count | entries`), which makes
  a batch atomic: after a crash either the whole batch is replayed or none
  of it.

Recovery rule: the first invalid frame ends the log. If it is in the last
segment, the file is truncated there and fsynced (a torn tail). If it is in
an earlier segment, startup fails with `ErrCorrupt`, because that data had
been fsynced and rotated and may have been acknowledged.

### fsync policy

| Policy | Ack means | Survives process crash | Survives power loss |
|---|---|---|---|
| `always` (default) | record fsynced | yes | yes |
| `interval` | record written to page cache, fsync every 100 ms | yes | all but the last interval |
| `never` | record written to page cache | yes | whatever the OS flushed |

Under `always`, concurrent writers share fsyncs (group commit): the first
waiter fsyncs everything written so far and waiters already covered return
without their own fsync. Writers are not blocked during an fsync, so the
next one covers all records written meanwhile.

On macOS, Go's `File.Sync` issues `F_FULLFSYNC`, which flushes the drive
cache and costs about 4 ms on the test machine. Linux `fsync` on NVMe is
usually much cheaper.

## Consequences

- A torn write is detected with certainty (short frame) or with CRC-32C
  probability (bit flips), and dropped without losing earlier records.
- Corruption in the middle of the active segment truncates everything after
  it, including records that may have been acknowledged. This is logged with
  the number of bytes dropped. Distinguishing a torn tail from mid-log media
  corruption would need a second checksum chain or a segment footer; it is
  listed in the roadmap.
- Tests: torn tails at several cut points, a flipped payload bit, an absurd
  length field, corruption in a non-final segment, a missing segment, and a
  SIGKILL test against a real server process with both a partial record and
  a bad-checksum record appended to the tail.
