"""vectordb-go, hnswlib, and FAISS on SIFT1M: one million real 128-dimensional vectors.

SIFT1M (Jegou et al., the standard ANN benchmark from corpus-texmex.irisa.fr) ships
its own queries and exact ground truth, so nothing here is generated. Every library
indexes the same 1,000,000 base vectors with M 16 and efConstruction 200, then answers
the first --queries query vectors at several ef values.

Builds use all cores. Queries are timed on one thread; hnswlib and FAISS take the
whole batch in one call, vectordb-go answers them one at a time inside the process.

    curl -O ftp://ftp.irisa.fr/local/texmex/corpus/sift.tar.gz && tar xzf sift.tar.gz
    make build
    uv run --project ../ann-vector-search-library python benchmarks/sift1m.py --data sift

Add --baseline-bin to also run an older vectordb binary on the same data.
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

ROOT = Path(__file__).resolve().parent.parent


def read_vecs(path: Path, dtype: str) -> np.ndarray:
    raw = np.fromfile(path, dtype=np.int32)
    dim = int(raw[0])
    return raw.reshape(-1, dim + 1)[:, 1:].copy().view(dtype)


def write_vecs(path: Path, rows: np.ndarray) -> None:
    n, d = rows.shape
    out = np.empty((n, d + 1), dtype=np.int32)
    out[:, 0] = d
    out[:, 1:] = rows.view(np.int32) if rows.dtype == np.float32 else rows.astype(np.int32)
    out.tofile(path)


def recall(pred: np.ndarray, truth: np.ndarray, k: int) -> float:
    hits = sum(len(set(map(int, p[:k])) & set(map(int, t[:k]))) for p, t in zip(pred, truth))
    return hits / (k * len(truth))


def per_query(fn, n: int, min_time: float = 2.0) -> float:
    fn()
    reps, start = 0, time.perf_counter()
    while True:
        fn()
        reps += 1
        if time.perf_counter() - start >= min_time:
            return (time.perf_counter() - start) / (reps * n)


def run_faiss(base, queries, truth, k):
    t0 = time.perf_counter()
    index = faiss.IndexFlatL2(base.shape[1])
    index.add(base)
    build = time.perf_counter() - t0
    faiss.omp_set_num_threads(1)
    pred = index.search(queries, k)[1]
    sec = per_query(lambda: index.search(queries, k), len(queries), min_time=1.0)
    return {"library": "FAISS IndexFlatL2 (exact, C++)", "build_s": build,
            "points": [{"ef": None, "recall": recall(pred, truth, k), "ms": sec * 1e3, "qps": 1 / sec}]}


def run_hnswlib(base, queries, truth, k, m, efc, efs, threads):
    index = hnswlib.Index(space="l2", dim=base.shape[1])
    index.init_index(max_elements=len(base), ef_construction=efc, M=m, random_seed=0)
    t0 = time.perf_counter()
    index.add_items(base, np.arange(len(base)), num_threads=threads)
    build = time.perf_counter() - t0
    points = []
    for ef in efs:
        index.set_ef(ef)
        pred = index.knn_query(queries, k=k, num_threads=1)[0]
        sec = per_query(lambda: index.knn_query(queries, k=k, num_threads=1), len(queries))
        points.append({"ef": ef, "recall": recall(pred, truth, k), "ms": sec * 1e3, "qps": 1 / sec})
    return {"library": "hnswlib (C++)", "build_s": build, "points": points}


def run_go(name, binary, files, k, m, efc, efs, threads):
    cmd = [str(binary), "bench", "--base", str(files["base"]), "--query-file", str(files["query"]),
           "--truth", str(files["truth"]), "--k", str(k), "--m", str(m),
           "--ef-construction", str(efc), "--ef", ",".join(map(str, efs)),
           "--workers", str(threads), "--min-time", "2s", "--json"]
    out = json.loads(subprocess.run(cmd, check=True, capture_output=True, text=True).stdout)
    points = [{"ef": r["ef"], "recall": r["recall"], "ms": r["mean_ms"], "qps": r["qps_single"],
               "p99_ms": r["p99_ms"]} for r in out["results"]]
    return {"library": name, "build_s": out["build_seconds"], "points": points}


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--data", required=True, help="directory with sift_base/query/groundtruth files")
    ap.add_argument("--n", type=int, default=1_000_000)
    ap.add_argument("--queries", type=int, default=1000)
    ap.add_argument("--k", type=int, default=10)
    ap.add_argument("--m", type=int, default=16)
    ap.add_argument("--ef-construction", type=int, default=200)
    ap.add_argument("--efs", default="16,32,64,128,256")
    ap.add_argument("--threads", type=int, default=os.cpu_count())
    ap.add_argument("--go-bin", default=str(ROOT / "bin" / "vectordb"))
    ap.add_argument("--baseline-bin", help="older vectordb binary to compare on the same data")
    ap.add_argument("--out", default=str(ROOT / "benchmarks" / "results" / "sift1m.json"))
    args = ap.parse_args()

    data = Path(args.data)
    base = read_vecs(data / "sift_base.fvecs", "float32")[: args.n]
    queries = read_vecs(data / "sift_query.fvecs", "float32")[: args.queries]
    truth = read_vecs(data / "sift_groundtruth.ivecs", "int32")[: args.queries]
    if args.n != 1_000_000:
        raise SystemExit("the shipped ground truth is for the full 1,000,000 vectors")
    efs = [int(e) for e in args.efs.split(",")]

    files = {"base": data / "sift_base.fvecs", "query": data / f"sift_query_{args.queries}.fvecs",
             "truth": data / f"sift_truth_{args.queries}.ivecs"}
    write_vecs(files["query"], queries)
    write_vecs(files["truth"], truth)

    runs = [run_faiss(base, queries, truth, args.k),
            run_hnswlib(base, queries, truth, args.k, args.m, args.ef_construction, efs, args.threads),
            run_go("vectordb-go (Go, NEON kernels)", args.go_bin, files, args.k, args.m,
                   args.ef_construction, efs, args.threads)]
    if args.baseline_bin:
        runs.append(run_go("vectordb-go (Go, previous portable loop)", args.baseline_bin, files, args.k,
                           args.m, args.ef_construction, efs, args.threads))

    report = {
        "dataset": "SIFT1M", "n": len(base), "dim": base.shape[1], "queries": len(queries), "k": args.k,
        "m": args.m, "ef_construction": args.ef_construction, "threads": args.threads,
        "machine": f"{platform.machine()} {platform.system()} {platform.release()}",
        "versions": {"hnswlib": metadata.version("hnswlib"), "faiss": faiss.__version__,
                     "numpy": np.__version__, "python": platform.python_version()},
        "runs": runs,
    }
    Path(args.out).parent.mkdir(parents=True, exist_ok=True)
    Path(args.out).write_text(json.dumps(report, indent=2) + "\n")

    print(f"\n### SIFT1M: {len(base):,} vectors, dim {base.shape[1]}, {len(queries):,} queries, "
          f"k={args.k}, M={args.m}, efConstruction={args.ef_construction}\n")
    print(f"| Library | Build ({args.threads} threads) | ef | Recall@{args.k} | Latency, 1 thread | QPS, 1 thread |")
    print("|---|---:|---:|---:|---:|---:|")
    for run in runs:
        for i, p in enumerate(run["points"]):
            build = f"{run['build_s']:.1f} s" if i == 0 else ""
            name = run["library"] if i == 0 else ""
            print(f"| {name} | {build} | {p['ef'] or 'exact'} | {p['recall']:.3f} | "
                  f"{p['ms']:.3f} ms | {p['qps']:,.0f} |")
    print(f"\nwrote {args.out}")


if __name__ == "__main__":
    main()
