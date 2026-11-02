"""Compare vectordb-go with hnswlib, FAISS and the pure-Python HNSW library.

Every library is measured on the same vectors, generated with the data module
of ann-vector-search-library (the same generator its own
compare_libraries.py uses), at matched HNSW parameters. Ground truth is an
exact float64 brute-force scan in numpy.

Both HNSW libraries are built twice, once single threaded and once with all
cores, to report both build times. Recall and query numbers come from the
single-threaded build because a parallel build is not deterministic.

Run from the vectordb-go repository root with the uv environment of the
Python library, which provides numpy, hnswlib and faiss-cpu:

    make build
    uv run --project ../ann-vector-search-library python benchmarks/compare.py

Results are printed as Markdown and written to benchmarks/results/.
"""

from __future__ import annotations

import argparse
import json
import os
import platform
import subprocess
import time
from importlib import metadata
from pathlib import Path

import faiss
import hnswlib
import numpy as np
from ann_vector_search_library.data import make_queries, make_vectors
from ann_vector_search_library.hnsw import HNSW

ROOT = Path(__file__).resolve().parent.parent


def write_vecs(path: Path, rows: np.ndarray) -> None:
    """Write fvecs (float32) or ivecs (int32): per row, int32 dim then values."""
    n, d = rows.shape
    out = np.empty((n, d + 1), dtype=np.int32)
    out[:, 0] = d
    out[:, 1:] = rows.view(np.int32) if rows.dtype == np.float32 else rows.astype(np.int32)
    out.tofile(path)


def exact_truth(base: np.ndarray, queries: np.ndarray, k: int) -> np.ndarray:
    b = base.astype(np.float64)
    q = queries.astype(np.float64)
    d = (q * q).sum(1)[:, None] - 2 * q @ b.T + (b * b).sum(1)[None, :]
    idx = np.argpartition(d, k, axis=1)[:, :k]
    order = np.take_along_axis(d, idx, 1).argsort(1)
    return np.take_along_axis(idx, order, 1).astype(np.int32)


def recall(pred: np.ndarray | list, truth: np.ndarray, k: int) -> float:
    hits = sum(len(set(map(int, p[:k])) & set(map(int, t[:k]))) for p, t in zip(pred, truth))
    return hits / (k * len(truth))


def timed_batches(fn, n_queries: int, min_time: float = 1.0) -> float:
    """Run fn() (which answers all queries) until min_time; return seconds per query."""
    fn()
    reps, start = 0, time.perf_counter()
    while True:
        fn()
        reps += 1
        elapsed = time.perf_counter() - start
        if elapsed >= min_time:
            return elapsed / (reps * n_queries)


def row(name, build1, buildn, rec, lat_ms, qps1, qpsn, note=""):
    return {
        "library": name,
        "build_1_thread_s": build1,
        "build_all_threads_s": buildn,
        "recall": rec,
        "latency_ms": lat_ms,
        "qps_1_thread": qps1,
        "qps_all_threads": qpsn,
        "note": note,
    }


def run_faiss(base, queries, truth, k, threads):
    t0 = time.perf_counter()
    index = faiss.IndexFlatL2(base.shape[1])
    index.add(base)
    build = time.perf_counter() - t0
    faiss.omp_set_num_threads(1)
    pred = index.search(queries, k)[1]
    per_q1 = timed_batches(lambda: index.search(queries, k), len(queries))
    faiss.omp_set_num_threads(threads)
    per_qn = timed_batches(lambda: index.search(queries, k), len(queries))
    return row("FAISS IndexFlatL2 (exact, C++)", build, build, recall(pred, truth, k),
               per_q1 * 1e3, 1 / per_q1, 1 / per_qn, "exact scan, BLAS")


def run_hnswlib(base, queries, truth, k, m, efc, ef, threads):
    builds = {}
    # Build with all threads first, then single threaded; queries run on the
    # single-threaded build, which is deterministic for a fixed seed.
    for nt in (threads, 1):
        index = hnswlib.Index(space="l2", dim=base.shape[1])
        index.init_index(max_elements=len(base), ef_construction=efc, M=m, random_seed=0)
        t0 = time.perf_counter()
        index.add_items(base, np.arange(len(base)), num_threads=nt)
        builds[nt] = time.perf_counter() - t0
    index.set_ef(ef)
    pred = index.knn_query(queries, k=k, num_threads=1)[0]
    per_q1 = timed_batches(lambda: index.knn_query(queries, k=k, num_threads=1), len(queries))
    per_qn = timed_batches(lambda: index.knn_query(queries, k=k, num_threads=threads), len(queries))
    return row("hnswlib (C++)", builds[1], builds[threads], recall(pred, truth, k),
               per_q1 * 1e3, 1 / per_q1, 1 / per_qn, "batched knn_query")


def run_python(vectors, queries, truth, k, m, efc, ef):
    t0 = time.perf_counter()
    index = HNSW(dim=len(vectors[0]), m=m, ef_construction=efc, seed=0)
    for i, v in enumerate(vectors):
        index.add(i, v)
    build = time.perf_counter() - t0
    pred = [[i for i, _ in index.search(q, k, ef=ef)] for q in queries]
    t0 = time.perf_counter()
    for q in queries:
        index.search(q, k, ef=ef)
    per_q = (time.perf_counter() - t0) / len(queries)
    return row("ann-vector-search-library (pure Python)", build, None, recall(pred, truth, k),
               per_q * 1e3, 1 / per_q, None, "single threaded by design")


def run_go(go_bin, files, k, m, efc, ef, threads):
    out = {}
    for workers in (1, threads):
        cmd = [str(go_bin), "bench", "--base", str(files["base"]), "--query-file", str(files["query"]),
               "--truth", str(files["truth"]), "--k", str(k), "--m", str(m),
               "--ef-construction", str(efc), "--ef", str(ef), "--workers", str(workers), "--json"]
        out[workers] = json.loads(subprocess.run(cmd, check=True, capture_output=True, text=True).stdout)
    # Queries run on the single-threaded build, as for hnswlib.
    r = out[1]["results"][0]
    return row("vectordb-go (Go)", out[1]["build_seconds"], out[threads]["build_seconds"],
               r["recall"], r["mean_ms"], r["qps_single"], r["qps_parallel"],
               f"p99 {r['p99_ms']:.3f} ms")


def fmt(v, spec):
    return "n/a" if v is None else format(v, spec)


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--sizes", default="10000,100000")
    ap.add_argument("--dim", type=int, default=16)
    ap.add_argument("--queries", type=int, default=200)
    ap.add_argument("--k", type=int, default=10)
    ap.add_argument("--m", type=int, default=16)
    ap.add_argument("--ef-construction", type=int, default=200)
    ap.add_argument("--ef", type=int, default=64)
    ap.add_argument("--python-max", type=int, default=10000,
                    help="skip the pure-Python index above this size (it takes minutes per 10k)")
    ap.add_argument("--threads", type=int, default=os.cpu_count())
    ap.add_argument("--go-bin", default=str(ROOT / "bin" / "vectordb"))
    ap.add_argument("--out", default=str(ROOT / "benchmarks" / "results"))
    args = ap.parse_args()

    out_dir = Path(args.out)
    data_dir = ROOT / "benchmarks" / "data"
    out_dir.mkdir(parents=True, exist_ok=True)
    data_dir.mkdir(parents=True, exist_ok=True)
    report = {
        "machine": f"{platform.machine()} {platform.system()} {platform.release()}",
        "processor": platform.processor(),
        "threads": args.threads,
        "params": vars(args),
        "versions": {"hnswlib": metadata.version("hnswlib"), "faiss": faiss.__version__,
                     "numpy": np.__version__, "python": platform.python_version()},
        "runs": [],
    }
    for n in [int(s) for s in args.sizes.split(",")]:
        vectors = make_vectors(n, args.dim, seed=0)
        query_list = make_queries(args.queries, args.dim, seed=1)
        base = np.asarray(vectors, dtype=np.float32)
        queries = np.asarray(query_list, dtype=np.float32)
        truth = exact_truth(base, queries, args.k)
        files = {name: data_dir / f"n{n}-d{args.dim}-{name}.{ext}"
                 for name, ext in (("base", "fvecs"), ("query", "fvecs"), ("truth", "ivecs"))}
        write_vecs(files["base"], base)
        write_vecs(files["query"], queries)
        write_vecs(files["truth"], truth)

        rows = [
            run_faiss(base, queries, truth, args.k, args.threads),
            run_hnswlib(base, queries, truth, args.k, args.m, args.ef_construction, args.ef, args.threads),
            run_go(args.go_bin, files, args.k, args.m, args.ef_construction, args.ef, args.threads),
        ]
        if n <= args.python_max:
            rows.append(run_python(vectors, query_list, truth, args.k, args.m, args.ef_construction, args.ef))
        report["runs"].append({"n": n, "dim": args.dim, "rows": rows})

        print(f"\n### {n:,} vectors, dim {args.dim}, {args.queries} queries, k={args.k}, "
              f"M={args.m}, efConstruction={args.ef_construction}, ef={args.ef}\n")
        print(f"| Library | Build, 1 thread | Build, {args.threads} threads | Recall@{args.k} "
              f"| Latency, 1 thread | QPS, 1 thread | QPS, {args.threads} threads |")
        print("|---|---:|---:|---:|---:|---:|---:|")
        for r in rows:
            print(f"| {r['library']} | {fmt(r['build_1_thread_s'], '.3f')} s | "
                  f"{fmt(r['build_all_threads_s'], '.3f')}{'' if r['build_all_threads_s'] is None else ' s'} | "
                  f"{r['recall']:.3f} | {r['latency_ms']:.4f} ms | {fmt(r['qps_1_thread'], ',.0f')} | "
                  f"{fmt(r['qps_all_threads'], ',.0f')} |")
        if n > args.python_max:
            print(f"\nThe pure-Python index was skipped at {n:,} vectors (above --python-max).")
    path = out_dir / f"compare-d{args.dim}.json"
    path.write_text(json.dumps(report, indent=2) + "\n")
    print(f"\nwrote {path.relative_to(ROOT)}")


if __name__ == "__main__":
    main()
