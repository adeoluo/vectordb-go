# AGENTS.md

Working notes for coding agents and contributors in **VectorDB Go**. The [README](README.md)
explains what the project does and why; this file explains how to change it safely.

## Project

A concurrent, persistent HNSW vector database server in Go, with a checksummed write-ahead log, snapshots, and crash recovery.

## Layout

- `benchmarks/`: benchmark scripts and recorded results
- `cmd/`: entry points for the binaries
- `docs/`: documentation, architecture diagram source and render, design records
- `internal/`: packages private to this module
- `scripts/`: maintenance scripts (release, assets, data)

Key modules:

- `cmd/vectordb`: Command vectordb runs the vector database server and its benchmark tool.
- `internal/cli`: Package cli implements the vectordb command line: serve and bench.
- `internal/dataset`: Package dataset generates deterministic synthetic vectors, reads and writes the standard fvecs and ivecs formats, and computes exact nearest neighbor ground truth by brute force.
- `internal/db`: Package db manages named collections, each an HNSW index made durable by a write-ahead log and periodic snapshots.
- `internal/hnsw`: Package hnsw implements a Hierarchical Navigable Small World graph (Malkov and Yashunin, 2016) for approximate nearest neighbor search over float32 vectors.
- `internal/server`: Package server exposes a db.DB over an HTTP JSON API.
- `internal/wal`: Package wal implements a segmented, append-only write-ahead log.

## Commands

| Command | Purpose |
|---|---|
| `make all` | Full local gate (mirror of continuous integration) |
| `make build` | Build the binaries |
| `make test` | Run the tests |
| `make race` | Tests with the race detector |
| `make vet` | Static checks with go vet |
| `make fmt` | Format the code |
| `make fmt-check` | Fail if code is not formatted |
| `make bench` | Run the benchmarks |
| `make compare` | Compare against the reference implementation |
| `make release` | Cross-compile release binaries into dist/ |
| `make docker` | Build the container image |
| `make clean` | Remove build output |
| `make changelog` | Regenerate CHANGELOG.md from Conventional Commits (git-cliff) |
| `make cut-release` | Cut a release locally: version bump, changelog, tag, GitHub release (RELEASE_VERSION=vX.Y.Z optional) |

Run the full local gate (`make all`) before committing; CI runs the same checks.

## Conventions

- Commits follow Conventional Commits (`feat:`, `fix:`, `perf:`, `refactor:`, `docs:`, `test:`, `ci:`, `chore:`).
  The changelog and the next version number are derived from them.
- Every number quoted in the README is produced by a command in this repository. When behavior
  changes, rerun that command and update the number in the same commit.
- When a component is added, removed or renamed, update `docs/architecture/architecture.mmd`
  and re-render `architecture.svg` so the diagram matches the code.
- README sections keep their fixed order. Prose avoids em and en dashes and promotional wording.
- Tests accompany behavior changes; a bug fix comes with a test that fails without it.
- Releases are cut with `make cut-release`.

## Go notes

- Code is formatted with `gofmt`; `go vet` and the race detector run in CI.
- Packages under `internal/` are not a public API; keep exported surface small.
- Errors are wrapped with context (`fmt.Errorf("...: %w", err)`), never discarded.
