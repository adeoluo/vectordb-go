package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adeoluo/vectordb-go/internal/dataset"
	"github.com/adeoluo/vectordb-go/internal/hnsw"
)

func TestBenchSynthetic(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"bench", "--n", "2000", "--dim", "8", "--queries", "50",
		"--ef", "32,64", "--min-time", "50ms", "--json"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	var rep benchReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.N != 2000 || len(rep.Results) != 2 || rep.Results[1].Recall < 0.9 || rep.Results[1].QPSSingle <= 0 {
		t.Fatalf("unexpected report %+v", rep)
	}
}

func TestBenchFromFiles(t *testing.T) {
	dir := t.TempDir()
	base := dataset.Clustered(1000, 4, 4, 0.2, 1)
	queries := dataset.Clustered(20, 4, 4, 0.2, 1)
	dataset.WriteFvecs(filepath.Join(dir, "base.fvecs"), base)
	dataset.WriteFvecs(filepath.Join(dir, "q.fvecs"), queries)
	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"bench", "--base", filepath.Join(dir, "base.fvecs"),
		"--query-file", filepath.Join(dir, "q.fvecs"), "--ef", "64", "--min-time", "20ms", "--workers", "1"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "n=1000 dim=4 queries=20") {
		t.Fatalf("output:\n%s", out.String())
	}
	truth := dataset.Exact(base, queries, 10, hnsw.L2)
	if len(truth) != 20 {
		t.Fatal("truth size")
	}
}

func TestUsageAndErrors(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Run(context.Background(), nil, &out, &errOut); code != 2 {
		t.Fatalf("no args: %d", code)
	}
	if code := Run(context.Background(), []string{"nope"}, &out, &errOut); code != 2 {
		t.Fatalf("unknown: %d", code)
	}
	if code := Run(context.Background(), []string{"serve"}, &out, &errOut); code != 1 {
		t.Fatalf("serve without data dir: %d", code)
	}
	if code := Run(context.Background(), []string{"serve", "--data-dir", t.TempDir(), "--fsync", "sometimes"}, &out, &errOut); code != 1 {
		t.Fatalf("bad fsync: %d", code)
	}
	out.Reset()
	if code := Run(context.Background(), []string{"version"}, &out, &errOut); code != 0 || !strings.HasPrefix(out.String(), "vectordb ") {
		t.Fatalf("version: %d %q", code, out.String())
	}
}
