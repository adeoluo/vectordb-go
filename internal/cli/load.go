package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/adeoluo/vectordb-go/internal/dataset"
)

// load drives a running server over HTTP: it creates a collection, upserts
// vectors in concurrent batches, then issues concurrent searches. It
// measures the whole path, including JSON encoding, the write-ahead log and
// fsync policy of the server under test.
func load(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("load", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "http://127.0.0.1:8080", "server base URL")
	name := fs.String("collection", "load", "collection to create (must not exist)")
	n := fs.Int("n", 100000, "vectors to upsert")
	dim := fs.Int("dim", 32, "vector dimension")
	batch := fs.Int("batch", 100, "vectors per upsert request")
	conc := fs.Int("concurrency", 8, "concurrent HTTP clients")
	queries := fs.Int("queries", 5000, "search requests to send")
	k := fs.Int("k", 10, "neighbors per search")
	ef := fs.Int("ef", 64, "search ef")
	if err := fs.Parse(args); err != nil {
		return err
	}
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: *conc}, Timeout: time.Minute}
	post := func(path string, body any) error {
		b, _ := json.Marshal(body)
		resp, err := client.Post(*url+path, "application/json", bytes.NewReader(b))
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		msg, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 300 {
			return fmt.Errorf("%s: %s: %s", path, resp.Status, bytes.TrimSpace(msg))
		}
		return nil
	}
	if err := post("/v1/collections", map[string]any{"name": *name, "dim": *dim}); err != nil {
		return err
	}
	all := dataset.Clustered(*n+*queries, *dim, 8, 0.15, 42)
	base, qs := all[:*n], all[*n:]

	type vec struct {
		ID     string    `json:"id"`
		Values []float32 `json:"values"`
	}
	var next atomic.Int64
	var firstErr atomic.Pointer[error]
	run := func(work func(i int) error, total int) time.Duration {
		next.Store(0)
		var wg sync.WaitGroup
		start := time.Now()
		for range *conc {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					i := int(next.Add(1) - 1)
					if i >= total || firstErr.Load() != nil {
						return
					}
					if err := work(i); err != nil {
						firstErr.CompareAndSwap(nil, &err)
						return
					}
				}
			}()
		}
		wg.Wait()
		return time.Since(start)
	}

	batches := (*n + *batch - 1) / *batch
	ingest := run(func(b int) error {
		lo, hi := b**batch, min((b+1)**batch, *n)
		vs := make([]vec, 0, hi-lo)
		for i := lo; i < hi; i++ {
			vs = append(vs, vec{ID: fmt.Sprintf("v%d", i), Values: base[i]})
		}
		return post("/v1/collections/"+*name+"/upsert", map[string]any{"vectors": vs})
	}, batches)
	if p := firstErr.Load(); p != nil {
		return *p
	}

	lat := make([]time.Duration, *queries)
	search := run(func(i int) error {
		t := time.Now()
		err := post("/v1/collections/"+*name+"/search", map[string]any{"vector": qs[i], "k": *k, "ef": *ef})
		lat[i] = time.Since(t)
		return err
	}, *queries)
	if p := firstErr.Load(); p != nil {
		return *p
	}
	if len(lat) == 0 {
		return errors.New("no queries")
	}
	slices.Sort(lat)
	ms := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
	fmt.Fprintf(stdout, "upsert: %d vectors (dim %d) in %d requests of %d, %d clients: %.2fs, %.0f vectors/s\n",
		*n, *dim, batches, *batch, *conc, ingest.Seconds(), float64(*n)/ingest.Seconds())
	fmt.Fprintf(stdout, "search: %d requests, k=%d ef=%d, %d clients: %.0f req/s, p50 %.2f ms, p99 %.2f ms\n",
		*queries, *k, *ef, *conc, float64(*queries)/search.Seconds(), ms(lat[len(lat)/2]), ms(lat[len(lat)*99/100]))
	return nil
}
