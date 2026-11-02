package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/adeoluo/vectordb-go/internal/wal"
)

// TestMain lets the test binary double as the server binary: when
// VECTORDB_CHILD is set it runs the CLI instead of the tests. The crash
// test starts it with os/exec so it can SIGKILL a real process.
func TestMain(m *testing.M) {
	if os.Getenv("VECTORDB_CHILD") == "1" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		code := Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
		stop()
		os.Exit(code)
	}
	os.Exit(m.Run())
}

type child struct {
	t   *testing.T
	cmd *exec.Cmd
	url string
}

type syncBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}

func startChild(t *testing.T, dataDir string, extra ...string) *child {
	t.Helper()
	addrFile := filepath.Join(dataDir, "addr")
	os.Remove(addrFile)
	args := append([]string{"serve", "--data-dir", dataDir, "--addr", "127.0.0.1:0", "--addr-file", addrFile}, extra...)
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "VECTORDB_CHILD=1")
	logs := &syncBuffer{}
	cmd.Stdout = logs
	cmd.Stderr = logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		if b, err := os.ReadFile(addrFile); err == nil && len(b) > 0 {
			c := &child{t: t, cmd: cmd, url: "http://" + string(b)}
			t.Cleanup(func() {
				if cmd.ProcessState == nil {
					cmd.Process.Kill()
					cmd.Wait()
				}
				if t.Failed() {
					logs.mu.Lock()
					t.Logf("child logs:\n%s", logs.String())
					logs.mu.Unlock()
				}
			})
			return c
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			cmd.Wait()
			t.Fatalf("server did not start:\n%s", logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (c *child) do(method, path string, body any, out any) (int, error) {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.url+path, r)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	if out != nil && resp.StatusCode < 300 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

func (c *child) must(method, path string, body any, out any) {
	c.t.Helper()
	code, err := c.do(method, path, body, out)
	if err != nil || code >= 300 {
		c.t.Fatalf("%s %s: code %d err %v", method, path, code, err)
	}
}

// vectorFor derives a vector deterministically from an id, so the test can
// check stored values without remembering them.
func vectorFor(id string, dim int) []float32 {
	h := uint32(2166136261)
	for i := 0; i < len(id); i++ {
		h = (h ^ uint32(id[i])) * 16777619
	}
	v := make([]float32, dim)
	for j := range v {
		h = h*1664525 + 1013904223
		v[j] = float32(h>>8)/float32(1<<24)*2 - 1
	}
	return v
}

type acked struct {
	mu  sync.Mutex
	ids []string
}

// hammer runs writers against the server until a request fails (which is
// how they notice the SIGKILL). Only batches that returned 200 are
// recorded as acknowledged.
func hammer(c *child, round string, writers, batch, dim int, ack *acked) *sync.WaitGroup {
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; ; n++ {
				ids := make([]string, batch)
				vecs := make([]map[string]any, batch)
				for i := range ids {
					ids[i] = fmt.Sprintf("%s-w%d-b%d-%d", round, w, n, i)
					vecs[i] = map[string]any{"id": ids[i], "values": vectorFor(ids[i], dim)}
				}
				code, err := c.do("POST", "/v1/collections/crash/upsert", map[string]any{"vectors": vecs}, nil)
				if err != nil || code != 200 {
					return
				}
				ack.mu.Lock()
				ack.ids = append(ack.ids, ids...)
				ack.mu.Unlock()
			}
		}()
	}
	return &wg
}

type statsResp struct {
	Vectors     int    `json:"vectors"`
	LastLSN     uint64 `json:"last_lsn"`
	SnapshotLSN uint64 `json:"snapshot_lsn"`
	Recovery    struct {
		SnapshotLSN     uint64 `json:"snapshot_lsn"`
		ReplayedRecords int    `json:"replayed_records"`
		TruncatedBytes  int64  `json:"truncated_bytes"`
	} `json:"recovery"`
}

func verifyAcked(t *testing.T, c *child, ids []string, dim int) {
	t.Helper()
	var wg sync.WaitGroup
	errs := make(chan string, len(ids))
	sem := make(chan struct{}, 16)
	for _, id := range ids {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			var got struct {
				Values []float32 `json:"values"`
			}
			code, err := c.do("GET", "/v1/collections/crash/vectors/"+id, nil, &got)
			if err != nil || code != 200 {
				errs <- fmt.Sprintf("%s: code %d err %v", id, code, err)
				return
			}
			want := vectorFor(id, dim)
			for j := range want {
				if math.Float32bits(got.Values[j]) != math.Float32bits(want[j]) {
					errs <- fmt.Sprintf("%s: value mismatch at %d", id, j)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	var lost []string
	for e := range errs {
		lost = append(lost, e)
	}
	if len(lost) > 0 {
		sort.Strings(lost)
		t.Fatalf("%d of %d acknowledged vectors lost or wrong after restart, first: %s", len(lost), len(ids), lost[0])
	}
}

func lastSegment(t *testing.T, dataDir string) string {
	t.Helper()
	segs, _ := filepath.Glob(filepath.Join(dataDir, "collections", "crash", "wal", "wal-*.log"))
	if len(segs) == 0 {
		t.Fatal("no WAL segments")
	}
	sort.Strings(segs)
	return segs[len(segs)-1]
}

// TestCrashRecovery runs the real server as a child process, writes to it
// from several goroutines, kills it with SIGKILL in the middle of the
// write stream, damages the log tail, and restarts it. Every write the
// server acknowledged must be present with identical values.
func TestCrashRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("crash recovery test starts subprocesses")
	}
	const dim, writers, batch = 8, 4, 10
	dataDir := t.TempDir()
	flags := []string{"--fsync", "always", "--snapshot-interval", "300ms", "--log-level", "warn"}
	var all []string

	c := startChild(t, dataDir, flags...)
	c.must("POST", "/v1/collections", map[string]any{"name": "crash", "dim": dim, "m": 8, "ef_construction": 64}, nil)

	for round, damage := range []string{"partial-record", "bad-checksum"} {
		ack := &acked{}
		wg := hammer(c, fmt.Sprintf("r%d", round), writers, batch, dim, ack)
		// Let writes and at least one background snapshot happen, then
		// kill the server while requests are in flight.
		time.Sleep(1500 * time.Millisecond)
		if err := c.cmd.Process.Signal(syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		c.cmd.Wait()
		wg.Wait()
		if ws, ok := c.cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || ws.Signal() != syscall.SIGKILL {
			t.Fatalf("child did not die from SIGKILL: %v", c.cmd.ProcessState)
		}
		all = append(all, ack.ids...)
		if len(ack.ids) == 0 {
			t.Fatal("no writes were acknowledged before the kill")
		}

		// Damage the tail of the active segment the way a crash during
		// write(2) or a bad sector would. Neither record was acknowledged.
		seg := lastSegment(t, dataDir)
		info, _ := os.Stat(seg)
		rec := wal.EncodeRecord(1<<40, []byte("unacknowledged record that never completed"))
		switch damage {
		case "partial-record":
			rec = rec[:len(rec)/2]
		case "bad-checksum":
			rec[len(rec)-1] ^= 0xff
		}
		f, _ := os.OpenFile(seg, os.O_APPEND|os.O_WRONLY, 0o644)
		f.Write(rec)
		f.Close()

		c = startChild(t, dataDir, flags...)
		var st statsResp
		c.must("GET", "/v1/collections/crash", nil, &st)
		t.Logf("round %d (%s): %d acked this round, %d total; server has %d vectors; recovered from snapshot LSN %d, replayed %d records, truncated %d bytes",
			round, damage, len(ack.ids), len(all), st.Vectors, st.Recovery.SnapshotLSN, st.Recovery.ReplayedRecords, st.Recovery.TruncatedBytes)
		if st.Recovery.TruncatedBytes != int64(len(rec)) {
			t.Fatalf("expected %d damaged bytes to be truncated, got %d", len(rec), st.Recovery.TruncatedBytes)
		}
		if after, _ := os.Stat(seg); after.Size() != info.Size() {
			t.Fatalf("segment size %d after recovery, want %d", after.Size(), info.Size())
		}
		// In-flight batches may or may not have been logged before the
		// kill, so the server can hold more than was acknowledged, never
		// fewer.
		if st.Vectors < len(all) {
			t.Fatalf("server has %d vectors, %d were acknowledged", st.Vectors, len(all))
		}
		verifyAcked(t, c, all, dim)
	}

	// The restarted server accepts writes, answers searches, and shuts down
	// gracefully on SIGTERM with a final snapshot.
	probe := all[len(all)/2]
	var res struct {
		Results []struct {
			ID       string  `json:"id"`
			Distance float32 `json:"distance"`
		} `json:"results"`
	}
	c.must("POST", "/v1/collections/crash/search", map[string]any{"vector": vectorFor(probe, dim), "k": 3, "ef": 64}, &res)
	if len(res.Results) == 0 || res.Results[0].ID != probe || res.Results[0].Distance != 0 {
		t.Fatalf("search for %s returned %+v", probe, res.Results)
	}
	c.must("POST", "/v1/collections/crash/upsert", map[string]any{"vectors": []map[string]any{{"id": "final", "values": vectorFor("final", dim)}}}, nil)
	all = append(all, "final")
	c.cmd.Process.Signal(syscall.SIGTERM)
	if err := c.cmd.Wait(); err != nil {
		t.Fatalf("graceful shutdown exited with %v", err)
	}

	c = startChild(t, dataDir, flags...)
	var st statsResp
	c.must("GET", "/v1/collections/crash", nil, &st)
	if st.Recovery.ReplayedRecords != 0 || st.Recovery.TruncatedBytes != 0 {
		t.Fatalf("after graceful shutdown expected a clean snapshot start, got %+v", st.Recovery)
	}
	verifyAcked(t, c, all, dim)
	c.cmd.Process.Signal(syscall.SIGTERM)
	c.cmd.Wait()
	if !strings.HasPrefix(c.url, "http://127.0.0.1:") {
		t.Fatalf("unexpected url %s", c.url)
	}
}
