package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"

	"github.com/adeoluo/vectordb-go/internal/dataset"
	"github.com/adeoluo/vectordb-go/internal/hnsw"
)

type efResult struct {
	Ef          int     `json:"ef"`
	Recall      float64 `json:"recall"`
	MeanMs      float64 `json:"mean_ms"`
	P50Ms       float64 `json:"p50_ms"`
	P99Ms       float64 `json:"p99_ms"`
	QPSSingle   float64 `json:"qps_single"`
	QPSParallel float64 `json:"qps_parallel"`
}

type benchReport struct {
	Dataset        string     `json:"dataset"`
	N              int        `json:"n"`
	Dim            int        `json:"dim"`
	Queries        int        `json:"queries"`
	K              int        `json:"k"`
	M              int        `json:"m"`
	EfConstruction int        `json:"ef_construction"`
	Metric         string     `json:"metric"`
	BuildWorkers   int        `json:"build_workers"`
	SearchWorkers  int        `json:"search_workers"`
	BuildSeconds   float64    `json:"build_seconds"`
	InsertsPerSec  float64    `json:"inserts_per_sec"`
	Results        []efResult `json:"results"`
}

func bench(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	fs.SetOutput(stderr)
	n := fs.Int("n", 10000, "number of synthetic base vectors")
	dim := fs.Int("dim", 16, "dimension of synthetic vectors")
	nq := fs.Int("queries", 200, "number of synthetic queries")
	clusters := fs.Int("clusters", 8, "clusters in the synthetic mixture")
	spread := fs.Float64("spread", 0.15, "standard deviation around each cluster center")
	seed := fs.Int64("seed", 0, "dataset seed")
	k := fs.Int("k", 10, "neighbors per query")
	m := fs.Int("m", 16, "HNSW M")
	efc := fs.Int("ef-construction", 200, "HNSW efConstruction")
	efList := fs.String("ef", "16,32,64,128,256", "comma separated search ef values")
	metricName := fs.String("metric", "l2", "distance metric: l2 or cosine")
	workers := fs.Int("workers", runtime.GOMAXPROCS(0), "goroutines used to build the index")
	minTime := fs.Duration("min-time", time.Second, "minimum measurement time per ef value")
	basePath := fs.String("base", "", "read base vectors from this fvecs file instead of generating them")
	queryPath := fs.String("query-file", "", "read queries from this fvecs file")
	truthPath := fs.String("truth", "", "read ground truth neighbor ids from this ivecs file")
	asJSON := fs.Bool("json", false, "print a JSON report")
	if err := fs.Parse(args); err != nil {
		return err
	}
	efs, err := splitInts(*efList)
	if err != nil {
		return err
	}
	metric, err := hnsw.ParseMetric(*metricName)
	if err != nil {
		return err
	}

	var base, queries [][]float32
	var truth [][]int32
	desc := ""
	if *basePath != "" {
		if *queryPath == "" {
			return errors.New("--query-file is required with --base")
		}
		if base, err = dataset.ReadFvecs(*basePath); err != nil {
			return err
		}
		if queries, err = dataset.ReadFvecs(*queryPath); err != nil {
			return err
		}
		desc = "file:" + *basePath
	} else {
		// Queries are held out from the same mixture as the base vectors.
		all := dataset.Clustered(*n+*nq, *dim, *clusters, *spread, *seed)
		base, queries = all[:*n], all[*n:]
		desc = fmt.Sprintf("clustered(clusters=%d, spread=%g, seed=%d, held-out queries)", *clusters, *spread, *seed)
	}
	if len(base) == 0 || len(queries) == 0 {
		return errors.New("empty dataset")
	}
	if *truthPath != "" {
		if truth, err = dataset.ReadIvecs(*truthPath); err != nil {
			return err
		}
		if len(truth) != len(queries) {
			return fmt.Errorf("truth has %d rows for %d queries", len(truth), len(queries))
		}
	} else {
		truth = dataset.Exact(base, queries, *k, metric)
	}

	ix, err := hnsw.New(hnsw.Config{Dim: len(base[0]), M: *m, EfConstruction: *efc, Metric: metric})
	if err != nil {
		return err
	}
	// orig maps node id back to the position in base, since concurrent
	// inserts do not assign ids in input order.
	orig := make([]int32, len(base))
	start := time.Now()
	var next atomic.Int64
	var wg sync.WaitGroup
	for range max(1, *workers) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := next.Add(1) - 1
				if i >= int64(len(base)) {
					return
				}
				id, err := ix.Insert(strconv.Itoa(int(i)), base[i])
				if err != nil {
					panic(err)
				}
				orig[id] = int32(i)
			}
		}()
	}
	wg.Wait()
	build := time.Since(start)

	rep := benchReport{
		Dataset: desc, N: len(base), Dim: len(base[0]), Queries: len(queries), K: *k,
		M: *m, EfConstruction: *efc, Metric: metric.String(),
		BuildWorkers: max(1, *workers), SearchWorkers: runtime.GOMAXPROCS(0),
		BuildSeconds: build.Seconds(), InsertsPerSec: float64(len(base)) / build.Seconds(),
	}
	for _, ef := range efs {
		rep.Results = append(rep.Results, measure(ix, orig, queries, truth, *k, ef, *minTime))
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	fmt.Fprintf(stdout, "dataset %s: n=%d dim=%d queries=%d k=%d metric=%s\n", rep.Dataset, rep.N, rep.Dim, rep.Queries, rep.K, rep.Metric)
	fmt.Fprintf(stdout, "build: M=%d efConstruction=%d workers=%d: %.3fs (%.0f inserts/s)\n\n",
		rep.M, rep.EfConstruction, rep.BuildWorkers, rep.BuildSeconds, rep.InsertsPerSec)
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintf(tw, "ef\trecall@%d\tmean ms\tp50 ms\tp99 ms\tQPS (1 goroutine)\tQPS (%d goroutines)\t\n", rep.K, rep.SearchWorkers)
	for _, r := range rep.Results {
		fmt.Fprintf(tw, "%d\t%.4f\t%.4f\t%.4f\t%.4f\t%.0f\t%.0f\t\n", r.Ef, r.Recall, r.MeanMs, r.P50Ms, r.P99Ms, r.QPSSingle, r.QPSParallel)
	}
	return tw.Flush()
}

func measure(ix *hnsw.Index, orig []int32, queries [][]float32, truth [][]int32, k, ef int, minTime time.Duration) efResult {
	predicted := make([][]int32, len(queries))
	for i, q := range queries {
		res, _ := ix.Search(q, k, ef)
		ids := make([]int32, len(res))
		for j, r := range res {
			ids[j] = orig[r.ID]
		}
		predicted[i] = ids
	}
	out := efResult{Ef: ef, Recall: dataset.Recall(predicted, truth, k)}

	// Single goroutine: per-query latency distribution.
	var lat []time.Duration
	start := time.Now()
	for time.Since(start) < minTime || len(lat) < len(queries) {
		for _, q := range queries {
			t := time.Now()
			ix.Search(q, k, ef)
			lat = append(lat, time.Since(t))
		}
	}
	total := time.Since(start)
	slices.Sort(lat)
	var sum time.Duration
	for _, d := range lat {
		sum += d
	}
	ms := func(d time.Duration) float64 { return float64(d.Nanoseconds()) / 1e6 }
	out.MeanMs = ms(sum / time.Duration(len(lat)))
	out.P50Ms = ms(lat[len(lat)/2])
	out.P99Ms = ms(lat[min(len(lat)-1, len(lat)*99/100)])
	out.QPSSingle = float64(len(lat)) / total.Seconds()

	// GOMAXPROCS goroutines: aggregate throughput.
	var done atomic.Int64
	var stop atomic.Bool
	var wg sync.WaitGroup
	start = time.Now()
	for w := range runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := w; !stop.Load(); i++ {
				ix.Search(queries[i%len(queries)], k, ef)
				done.Add(1)
			}
		}()
	}
	time.Sleep(minTime)
	stop.Store(true)
	wg.Wait()
	out.QPSParallel = float64(done.Load()) / time.Since(start).Seconds()
	return out
}
