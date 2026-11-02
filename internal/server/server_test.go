package server_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/adeoluo/vectordb-go/internal/db"
	"github.com/adeoluo/vectordb-go/internal/server"
	"github.com/adeoluo/vectordb-go/internal/wal"
)

func setup(t *testing.T) *httptest.Server {
	t.Helper()
	d, err := db.Open(db.Options{DataDir: t.TempDir(), Sync: wal.SyncNever})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.New(d, slog.New(slog.NewTextHandler(io.Discard, nil))))
	t.Cleanup(func() {
		ts.Close()
		d.Close()
	})
	return ts
}

func call(t *testing.T, ts *httptest.Server, method, path, body string, wantCode int) map[string]any {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantCode {
		t.Fatalf("%s %s: status %d, want %d: %s", method, path, resp.StatusCode, wantCode, raw)
	}
	out := map[string]any{}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
	} else {
		out["text"] = string(raw)
	}
	return out
}

func TestAPI(t *testing.T) {
	ts := setup(t)
	call(t, ts, "GET", "/health", "", 200)
	call(t, ts, "POST", "/v1/collections", `{"name":"docs","dim":3,"metric":"l2"}`, 201)
	call(t, ts, "POST", "/v1/collections", `{"name":"docs","dim":3}`, 409)
	call(t, ts, "POST", "/v1/collections", `{"name":"x","dim":3,"bogus":1}`, 400)
	call(t, ts, "POST", "/v1/collections", `{"name":"x","dim":-1}`, 400)
	call(t, ts, "POST", "/v1/collections", `not json`, 400)

	up := call(t, ts, "POST", "/v1/collections/docs/upsert",
		`{"vectors":[{"id":"a","values":[0,0,0]},{"id":"b","values":[1,0,0]},{"id":"c","values":[5,5,5]}]}`, 200)
	if up["upserted"].(float64) != 3 || up["lsn"].(float64) != 1 {
		t.Fatalf("upsert response %v", up)
	}
	call(t, ts, "POST", "/v1/collections/docs/upsert", `{"vectors":[{"id":"d","values":[1,2]}]}`, 400)
	call(t, ts, "POST", "/v1/collections/missing/upsert", `{"vectors":[]}`, 404)

	res := call(t, ts, "POST", "/v1/collections/docs/search", `{"vector":[0.9,0,0],"k":2}`, 200)
	hits := res["results"].([]any)
	if len(hits) != 2 || hits[0].(map[string]any)["id"] != "b" || hits[1].(map[string]any)["id"] != "a" {
		t.Fatalf("search results %v", hits)
	}
	call(t, ts, "POST", "/v1/collections/docs/search", `{"vector":[0,0],"k":2}`, 400)
	call(t, ts, "POST", "/v1/collections/docs/search", `{"vector":[0,0,0],"k":0}`, 400)

	v := call(t, ts, "GET", "/v1/collections/docs/vectors/c", "", 200)
	if vals := v["values"].([]any); len(vals) != 3 || vals[0].(float64) != 5 {
		t.Fatalf("get vector %v", v)
	}
	call(t, ts, "GET", "/v1/collections/docs/vectors/zzz", "", 404)

	del := call(t, ts, "POST", "/v1/collections/docs/delete", `{"ids":["b","nope"]}`, 200)
	if del["deleted"].(float64) != 1 {
		t.Fatalf("delete response %v", del)
	}
	res = call(t, ts, "POST", "/v1/collections/docs/search", `{"vector":[0.9,0,0],"k":1}`, 200)
	if id := res["results"].([]any)[0].(map[string]any)["id"]; id != "a" {
		t.Fatalf("deleted vector still returned: %v", id)
	}

	snap := call(t, ts, "POST", "/v1/collections/docs/snapshot", "", 200)
	if snap["snapshot_lsn"].(float64) != 2 {
		t.Fatalf("snapshot %v", snap)
	}
	st := call(t, ts, "GET", "/v1/collections/docs", "", 200)
	if st["vectors"].(float64) != 2 || st["tombstones"].(float64) != 1 || st["snapshot_lsn"].(float64) != 2 {
		t.Fatalf("stats %v", st)
	}
	list := call(t, ts, "GET", "/v1/collections", "", 200)
	if len(list["collections"].([]any)) != 1 {
		t.Fatalf("list %v", list)
	}

	m := call(t, ts, "GET", "/metrics", "", 200)["text"].(string)
	for _, want := range []string{
		`vectordb_collection_vectors{collection="docs"} 2`,
		`vectordb_http_requests_total{route="POST /v1/collections/{name}/search",code="200"} 2`,
		`vectordb_http_request_duration_seconds_count{route="GET /health"} 1`,
		`vectordb_snapshots_total{collection="docs"} 1`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics missing %q", want)
		}
	}

	call(t, ts, "DELETE", "/v1/collections/docs", "", 200)
	call(t, ts, "GET", "/v1/collections/docs", "", 404)
	call(t, ts, "GET", "/nope", "", 404)
}

func TestBodyLimit(t *testing.T) {
	ts := setup(t)
	call(t, ts, "POST", "/v1/collections", `{"name":"big","dim":2}`, 201)
	var b bytes.Buffer
	b.WriteString(`{"vectors":[`)
	for b.Len() < server.MaxBodyBytes+1024 {
		b.WriteString(`{"id":"xxxxxxxxxxxxxxxx","values":[1,2]},`)
	}
	b.WriteString(`{"id":"y","values":[1,2]}]}`)
	call(t, ts, "POST", "/v1/collections/big/upsert", b.String(), 413)
}
