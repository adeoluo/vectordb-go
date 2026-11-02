# ADR 0001: Per-node locks and an atomically published node table

## Status

Accepted.

## Context

The server must answer searches while it ingests. HNSW insertion is
expensive (an `efConstruction`-wide search on every layer plus neighbor
pruning), so serializing inserts behind one lock limits ingest to one core,
and a reader-writer lock over the whole index would also stall every search
for the duration of each insert.

Options considered:

1. **One `sync.RWMutex` for the index.** Simple and obviously correct.
   Searches run in parallel, but every insert excludes all searches and all
   other inserts.
2. **Per-node locks (the approach hnswlib uses).** Inserts lock only the
   adjacency list they are modifying. Searches take a short read lock per
   visited node.
3. **Lock-free adjacency lists** (copy-on-write slices behind atomic
   pointers). No read locks, but every edge update allocates, and pruning
   under contention needs retry loops.

## Decision

Option 2, with the following rules:

- Nodes are stored in chunks of 1024. Vectors and bottom-layer adjacency
  lists are flat arrays inside the chunk. The table of chunk pointers is a
  copy-on-grow slice published through `atomic.Pointer`, so readers never
  lock to find a node. `Allocate` appends under `allocMu`.
- A node's key, level and vector are written before the node is reachable
  and never change afterwards, so distance computations take no locks.
- Each node has a `sync.RWMutex` that guards only its adjacency lists.
  Readers copy the list under the read lock into a per-search buffer and
  release the lock before computing any distances.
- A goroutine holds **at most one node lock at a time**. When an insert
  adds a back link from neighbor `n` to the new node, it locks `n`, reads
  the vectors it needs (immutable, so no further locks), prunes, writes, and
  unlocks. There is no lock ordering to get wrong and no deadlock between
  inserts.
- The entry point and maximum level are guarded by a small `RWMutex` held
  only to read or write those two words.
- Tombstones are an `atomic.Bool` per node.

Why it is safe to read the node table without a lock: a goroutine can only
learn about node `i` by reading a link to it. That link was written under
the neighbor's mutex after `Allocate(i)` stored the new table pointer, so
the mutex release/acquire pair orders the read after the publication. If a
reader's cached table is too short for `i`, it reloads the atomic pointer
and is guaranteed to see the node.

## Consequences

- Ingest scales with cores. On an 8-core Apple M1, building 100,000 dim-16
  vectors takes 11.6 s on one goroutine and 2.2 s on eight
  (see the README benchmarks).
- Searches never wait for an insert to finish, only for the few
  nanoseconds a neighbor list is being rewritten.
- A per-node `RWMutex` costs 24 bytes per node and two atomic operations per
  visited node during search. This is visible in profiles but small next to
  distance computations.
- Concurrent inserts make the graph shape depend on scheduling, so a
  parallel build is not bit-for-bit reproducible. A single-goroutine build
  with a fixed seed is. Snapshots capture the exact graph, so restarts do
  not change search results.
- `go test -race` runs a test where eight writers and eight readers share
  one index, followed by a recall check that would fail if links were lost.
