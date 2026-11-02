# ADR 0004: Chunked contiguous storage for vectors and bottom-layer links

## Status

Accepted. Supersedes the first implementation, which stored each node as a
separate heap object.

## Context

The first version kept a `[]*node` table where every node owned a vector
slice and a slice of adjacency slices. It matched hnswlib on recall, but a
CPU profile of a 100,000 vector benchmark showed:

- most of the search time in `l2sq` and `searchLayer` was spent waiting on
  memory: node pointer, then slice header, then vector data, three dependent
  loads per distance;
- `runtime.madvise` and span allocation at about 12% of CPU during a build,
  from several small allocations per inserted node;
- a 32-bit visited array (400 KB at 100,000 nodes) adding cache pressure.

## Decision

- Store nodes in chunks of 1024. Each chunk has one flat `[]float32` for its
  vectors and one flat `[]uint32` for bottom-layer adjacency lists (a count
  slot followed by up to `2*M` ids). Upper layers, which hold about 1/M of
  the nodes, keep ordinary slices.
- Per-node locks and tombstone flags live in fixed arrays inside the chunk.
- The visited set uses 16-bit generation stamps.
- Search scratch buffers are recycled through a bounded channel instead of
  `sync.Pool`, which is emptied at every GC cycle and was causing the
  visited array to be reallocated repeatedly.

The on-disk snapshot format did not change.

## Consequences

Measured on the 100,000 vector dim-16 dataset from the comparison benchmark,
same machine, same session:

| | Before | After |
|---|---:|---:|
| Build, 1 goroutine | 18.1 s | 13.6 s |
| Build, 8 goroutines | 3.87 s | 2.15 s |
| Mean query latency, ef 64 | 0.044 ms | 0.034 ms |

Steady-state search makes one allocation (the result slice). The cost is a
little more code in the index: reads and writes of the bottom layer go
through helper methods that compute the chunk and offset.
