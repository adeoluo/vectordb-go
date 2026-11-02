# ADR 0003: Snapshots of the full graph plus WAL replay

## Status

Accepted.

## Context

Replaying the whole log at startup means re-running HNSW insertion for every
vector ever written. For 100,000 dim-32 vectors that takes about 3.5 s on
the test machine even with parallel linking, and it grows with history
(overwritten and deleted vectors are replayed too). The log also grows
without bound.

## Decision

- A snapshot stores the **graph itself**: configuration, entry point, and for
  every node its key, level, tombstone flag, vector and adjacency lists.
  Loading it is a sequential read with no distance computations.
- A snapshot is a consistent cut at an LSN `L`. The collection holds its
  write mutex, waits for in-flight links to finish, rotates the WAL so the
  new segment starts at `L + 1`, and serializes the graph to memory. The
  mutex is then released and the buffer is written to disk.
- Files are written as `snapshot-<L>.snap.tmp`, fsynced, renamed, and the
  directory fsynced. A crash at any point leaves either the old set of
  snapshots or the new one. Stray `.tmp` files are removed at startup.
- The whole file is covered by a trailing CRC-32C. The decoder also bounds
  checks every link, level and length, so a damaged file is rejected rather
  than loaded into an index that could panic later.
- Two snapshots are retained, and WAL segments are removed only up to the
  older one. If the newest snapshot fails its checksum it is renamed to
  `.corrupt` and recovery continues from the older one plus replay.
- Triggers: elapsed time with new writes (`--snapshot-interval`, default
  5 min), WAL bytes since the last snapshot (`--snapshot-wal-bytes`, default
  256 MiB), graceful shutdown (`--snapshot-on-close`, default on), and
  `POST /v1/collections/{name}/snapshot`.

## Consequences

- Restart of a 100,000 vector dim-32 collection: 49 ms from a snapshot
  versus 3.46 s from log replay alone.
- Restoring from a snapshot reproduces the exact graph, so search results
  are identical before and after a restart. A test checks this directly, and
  checks that snapshot-only, snapshot-plus-replay, and replay-only recovery
  all yield the same set of live vectors.
- Writes pause while the graph is copied into memory (tens of milliseconds
  at 100,000 vectors). Searches do not pause.
- Peak memory during a snapshot is roughly twice the encoded index size.
  Streaming the encoding with copy-on-write adjacency lists would remove the
  copy; it is not needed at the sizes this project targets.
- Tombstoned nodes are kept in snapshots because the graph routes through
  them. Reclaiming them requires a rebuild (roadmap).
