// Package hnsw implements a Hierarchical Navigable Small World graph
// (Malkov and Yashunin, 2016) for approximate nearest neighbor search over
// float32 vectors.
//
// # Concurrency
//
// The index is safe for concurrent use by any number of readers and writers.
// It does not use a single index-wide lock. Instead:
//
//   - Nodes are stored in fixed-size chunks. The chunk table is a
//     copy-on-grow slice published through an atomic.Pointer, so readers
//     load it without locking; Allocate appends under allocMu.
//   - Each node has its own sync.RWMutex that guards only its adjacency
//     lists. Vectors, keys and levels are written once before a node becomes
//     reachable, so distance computations take no locks.
//   - The entry point and maximum level are guarded by a small RWMutex that is
//     held only long enough to read or update two words.
//
// At most one node lock is held at any time, so there is no lock ordering to
// get wrong and no possibility of deadlock between inserts. See
// docs/adr/0001-locking-strategy.md for the trade-offs.
package hnsw

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
)

// Config holds the construction parameters of an index.
type Config struct {
	Dim            int
	M              int
	EfConstruction int
	Metric         Metric
	Seed           int64
}

// Default parameter values, matched to the Python ann-vector-search-library
// comparison so the two implementations can be measured side by side.
const (
	DefaultM              = 16
	DefaultEfConstruction = 200
)

// Validate checks the configuration and fills in defaults.
func (c *Config) Validate() error {
	if c.Dim <= 0 || c.Dim > 65536 {
		return fmt.Errorf("dimension must be in [1, 65536], got %d", c.Dim)
	}
	if c.M == 0 {
		c.M = DefaultM
	}
	if c.EfConstruction == 0 {
		c.EfConstruction = DefaultEfConstruction
	}
	if c.M < 2 || c.M > 256 {
		return fmt.Errorf("M must be in [2, 256], got %d", c.M)
	}
	if c.EfConstruction < 1 || c.EfConstruction > 10000 {
		return fmt.Errorf("ef_construction must be in [1, 10000], got %d", c.EfConstruction)
	}
	if c.Metric != L2 && c.Metric != Cosine {
		return fmt.Errorf("unknown metric %d", c.Metric)
	}
	return nil
}

// ErrDimension is returned when a vector does not match the index dimension.
var ErrDimension = errors.New("vector dimension mismatch")

const (
	chunkBits = 10
	chunkSize = 1 << chunkBits
	chunkMask = chunkSize - 1
)

// chunk stores chunkSize consecutive nodes. Vectors and bottom-layer
// adjacency lists live in flat arrays, so a distance computation touches one
// contiguous slice instead of chasing a per-node pointer, and building an
// index allocates once per 1024 nodes instead of several times per node.
// This mirrors the memory layout hnswlib uses and was introduced after
// profiling showed cache misses and allocation as the main costs at 100k
// vectors.
type chunk struct {
	vecs    []float32 // chunkSize * dim
	l0      []uint32  // chunkSize * (mMax0 + 1); slot 0 of each node is its count
	locks   [chunkSize]sync.RWMutex
	deleted [chunkSize]atomic.Bool
	meta    [chunkSize]nodeMeta
}

// nodeMeta is written once by Allocate before the node can be reached. The
// contents of upper are guarded by the node lock.
type nodeMeta struct {
	key   string
	level int
	upper [][]uint32 // upper[l-1] holds the links at layer l >= 1
}

// Index is a concurrent HNSW graph.
type Index struct {
	cfg       Config
	mMax0     int
	l0Stride  int
	levelMult float64
	dist      func(a, b []float32) float32

	allocMu sync.Mutex
	rng     *rand.Rand
	n       atomic.Uint32
	chunks  atomic.Pointer[[]*chunk]

	epMu     sync.RWMutex
	entry    int64
	maxLevel int

	live atomic.Int64
	// free recycles search buffers. A buffered channel is used instead of
	// sync.Pool because the pool is emptied on every GC cycle, and
	// reallocating the visited array after each cycle showed up as a
	// significant cost in profiles.
	free chan *scratch
}

// Result is a single search hit.
type Result struct {
	ID       uint32
	Key      string
	Distance float32
}

// New creates an empty index.
func New(cfg Config) (*Index, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	ix := &Index{
		cfg:       cfg,
		mMax0:     2 * cfg.M,
		l0Stride:  2*cfg.M + 1,
		levelMult: 1 / math.Log(float64(cfg.M)),
		rng:       rand.New(rand.NewSource(cfg.Seed)),
		entry:     -1,
	}
	if cfg.Metric == Cosine {
		ix.dist = cosineDist
	} else {
		ix.dist = l2sq
	}
	empty := make([]*chunk, 0, 16)
	ix.chunks.Store(&empty)
	ix.free = make(chan *scratch, 4*runtime.GOMAXPROCS(0))
	return ix, nil
}

// Config returns the index configuration.
func (ix *Index) Config() Config { return ix.cfg }

// Len returns the number of allocated nodes, including tombstoned ones.
func (ix *Index) Len() int { return int(ix.n.Load()) }

// Live returns the number of nodes that are not tombstoned.
func (ix *Index) Live() int { return int(ix.live.Load()) }

// view caches the chunk table for the duration of one operation and
// reloads it only when an id beyond the cached chunks is encountered. A link
// to a node is always published after that node was allocated, so
// reloading is guaranteed to find it.
type view struct {
	ix     *Index
	chunks []*chunk
	dim    int
	stride int
}

func (ix *Index) view() view {
	return view{ix: ix, chunks: *ix.chunks.Load(), dim: ix.cfg.Dim, stride: ix.l0Stride}
}

func (v *view) chunk(id uint32) *chunk {
	c := int(id >> chunkBits)
	if c >= len(v.chunks) {
		v.chunks = *v.ix.chunks.Load()
	}
	return v.chunks[c]
}

func (v *view) vec(id uint32) []float32 {
	off := int(id&chunkMask) * v.dim
	return v.chunk(id).vecs[off : off+v.dim : off+v.dim]
}

func (v *view) deleted(id uint32) bool { return v.chunk(id).deleted[id&chunkMask].Load() }

func (v *view) meta(id uint32) *nodeMeta { return &v.chunk(id).meta[id&chunkMask] }

// links copies the adjacency list of id at level into buf.
func (v *view) links(id uint32, level int, buf []uint32) []uint32 {
	ch := v.chunk(id)
	off := id & chunkMask
	ch.locks[off].RLock()
	if level == 0 {
		base := int(off) * v.stride
		cnt := int(ch.l0[base])
		buf = append(buf[:0], ch.l0[base+1:base+1+cnt]...)
	} else {
		buf = append(buf[:0], ch.meta[off].upper[level-1]...)
	}
	ch.locks[off].RUnlock()
	return buf
}

// setLinksLocked overwrites the adjacency list; the node lock must be held.
func (v *view) setLinksLocked(ch *chunk, off uint32, level int, ids []uint32) {
	if level == 0 {
		base := int(off) * v.stride
		ch.l0[base] = uint32(len(ids))
		copy(ch.l0[base+1:], ids)
		return
	}
	m := &ch.meta[off]
	m.upper[level-1] = append(m.upper[level-1][:0], ids...)
}

func (ix *Index) getScratch() *scratch {
	select {
	case s := <-ix.free:
		return s
	default:
		return &scratch{}
	}
}

func (ix *Index) putScratch(s *scratch) {
	select {
	case ix.free <- s:
	default:
	}
}

func (ix *Index) randomLevel() int {
	r := ix.rng.Float64()
	if r == 0 {
		r = 1e-12
	}
	return int(-math.Log(r) * ix.levelMult)
}

func (ix *Index) maxM(level int) int {
	if level == 0 {
		return ix.mMax0
	}
	return ix.cfg.M
}

// Insert adds a vector and links it into the graph. It is equivalent to
// Allocate followed by Link.
func (ix *Index) Insert(key string, vec []float32) (uint32, error) {
	id, err := ix.Allocate(key, vec)
	if err != nil {
		return 0, err
	}
	ix.Link(id)
	return id, nil
}

// Allocate copies vec into a new node and returns its id without linking it
// into the graph. The node is invisible to searches until Link is called.
// Splitting the two lets a caller assign ids in a fixed order (for example
// under a write-ahead-log lock) and then link many nodes in parallel.
func (ix *Index) Allocate(key string, vec []float32) (uint32, error) {
	if len(vec) != ix.cfg.Dim {
		return 0, fmt.Errorf("%w: expected %d, got %d", ErrDimension, ix.cfg.Dim, len(vec))
	}
	ix.allocMu.Lock()
	defer ix.allocMu.Unlock()
	id, err := ix.appendLocked(key, ix.randomLevel(), vec, ix.cfg.Metric == Cosine)
	if err != nil {
		return 0, err
	}
	ix.live.Add(1)
	return id, nil
}

// appendLocked reserves the next id and initializes its metadata and
// vector. allocMu must be held.
func (ix *Index) appendLocked(key string, level int, vec []float32, norm bool) (uint32, error) {
	n := ix.n.Load()
	if n == math.MaxUint32 {
		return 0, errors.New("index is full")
	}
	chunks := *ix.chunks.Load()
	if int(n>>chunkBits) == len(chunks) {
		ch := &chunk{
			vecs: make([]float32, chunkSize*ix.cfg.Dim),
			l0:   make([]uint32, chunkSize*ix.l0Stride),
		}
		grown := make([]*chunk, len(chunks), len(chunks)+1)
		copy(grown, chunks)
		grown = append(grown, ch)
		ix.chunks.Store(&grown)
		chunks = grown
	}
	ch := chunks[n>>chunkBits]
	off := n & chunkMask
	m := &ch.meta[off]
	m.key = key
	m.level = level
	if level > 0 {
		m.upper = make([][]uint32, level)
		for l := range m.upper {
			m.upper[l] = make([]uint32, 0, ix.cfg.M+1)
		}
	}
	o := int(off) * ix.cfg.Dim
	dst := ch.vecs[o : o+ix.cfg.Dim]
	copy(dst, vec)
	if norm {
		normalize(dst)
	}
	ix.n.Store(n + 1)
	return n, nil
}

// Link connects an allocated node into the graph. It is safe to call Link
// for different nodes concurrently.
func (ix *Index) Link(id uint32) {
	v := ix.view()
	level := v.meta(id).level

	ix.epMu.Lock()
	if ix.entry < 0 {
		ix.entry = int64(id)
		ix.maxLevel = level
		ix.epMu.Unlock()
		return
	}
	ep := uint32(ix.entry)
	maxL := ix.maxLevel
	ix.epMu.Unlock()

	s := ix.getScratch()
	q := v.vec(id)
	cur := ep
	curDist := ix.dist(q, v.vec(ep))
	for l := maxL; l > level; l-- {
		cur, curDist = ix.greedy(&v, s, q, cur, curDist, l)
	}
	ch, off := v.chunk(id), id&chunkMask
	for l := min(level, maxL); l >= 0; l-- {
		found := ix.searchLayer(&v, s, q, cur, curDist, l, ix.cfg.EfConstruction, false)
		if len(found) > 0 && found[0].id != id {
			cur, curDist = found[0].id, found[0].dist
		}
		s.sel = ix.selectNeighbors(&v, s.sel[:0], found, ix.cfg.M, id)
		s.nbuf = s.nbuf[:0]
		for _, c := range s.sel {
			s.nbuf = append(s.nbuf, c.id)
		}
		ch.locks[off].Lock()
		v.setLinksLocked(ch, off, l, s.nbuf)
		ch.locks[off].Unlock()
		for _, c := range s.sel {
			ix.connect(&v, s, c.id, id, c.dist, l)
		}
	}
	ix.putScratch(s)

	if level > maxL {
		ix.epMu.Lock()
		if level > ix.maxLevel {
			ix.entry = int64(id)
			ix.maxLevel = level
		}
		ix.epMu.Unlock()
	}
}

// connect adds a back link src -> target at level, pruning src's adjacency
// list with the neighbor selection heuristic when it overflows.
func (ix *Index) connect(v *view, s *scratch, src, target uint32, dist float32, level int) {
	ch, off := v.chunk(src), src&chunkMask
	limit := ix.maxM(level)
	ch.locks[off].Lock()
	defer ch.locks[off].Unlock()
	var links []uint32
	if level == 0 {
		base := int(off) * v.stride
		links = ch.l0[base+1 : base+1+int(ch.l0[base])]
	} else {
		links = ch.meta[off].upper[level-1]
	}
	if slices.Contains(links, target) {
		return
	}
	if len(links) < limit {
		if level == 0 {
			base := int(off) * v.stride
			ch.l0[base+1+len(links)] = target
			ch.l0[base]++
		} else {
			ch.meta[off].upper[level-1] = append(links, target)
		}
		return
	}
	srcVec := v.vec(src)
	cands := s.prune[:0]
	cands = append(cands, candidate{dist: dist, id: target})
	for _, other := range links {
		cands = append(cands, candidate{dist: ix.dist(srcVec, v.vec(other)), id: other})
	}
	slices.SortFunc(cands, func(a, b candidate) int {
		switch {
		case a.dist < b.dist:
			return -1
		case a.dist > b.dist:
			return 1
		}
		return 0
	})
	sel := ix.selectNeighbors(v, s.out[:0], cands, limit, src)
	ids := s.ids[:0]
	for _, c := range sel {
		ids = append(ids, c.id)
	}
	v.setLinksLocked(ch, off, level, ids)
	s.prune, s.out, s.ids = cands, sel, ids
}

// selectNeighbors applies the HNSW heuristic: walking candidates in order of
// increasing distance, a candidate is kept only if it is closer to the base
// point than to every neighbor already kept. This spreads edges around the
// point instead of clustering them in one direction. cands must be sorted by
// ascending distance. self is excluded.
func (ix *Index) selectNeighbors(v *view, dst, cands []candidate, m int, self uint32) []candidate {
	for _, c := range cands {
		if len(dst) >= m {
			break
		}
		if c.id == self {
			continue
		}
		cv := v.vec(c.id)
		keep := true
		for _, chosen := range dst {
			if ix.dist(cv, v.vec(chosen.id)) < c.dist {
				keep = false
				break
			}
		}
		if keep {
			dst = append(dst, c)
		}
	}
	return dst
}

func (ix *Index) greedy(v *view, s *scratch, q []float32, cur uint32, curDist float32, level int) (uint32, float32) {
	for changed := true; changed; {
		changed = false
		s.nbuf = v.links(cur, level, s.nbuf)
		for _, nb := range s.nbuf {
			if d := ix.dist(q, v.vec(nb)); d < curDist {
				cur, curDist, changed = nb, d, true
			}
		}
	}
	return cur, curDist
}

// searchLayer runs the bounded best-first search at one layer and returns up
// to ef candidates sorted by ascending distance. When liveOnly is set,
// tombstoned nodes are traversed (they still carry graph connectivity) but
// never returned. The returned slice aliases s.out.
func (ix *Index) searchLayer(v *view, s *scratch, q []float32, ep uint32, epDist float32, level, ef int, liveOnly bool) []candidate {
	s.vis.reset(int(ix.n.Load()))
	s.cand = s.cand[:0]
	s.res = s.res[:0]
	s.vis.visit(ep)
	s.cand.push(candidate{epDist, ep})
	if !liveOnly || !v.deleted(ep) {
		s.res.push(candidate{epDist, ep})
	}
	for len(s.cand) > 0 {
		c := s.cand.pop()
		if len(s.res) >= ef && c.dist > s.res.top().dist {
			break
		}
		s.nbuf = v.links(c.id, level, s.nbuf)
		for _, nbID := range s.nbuf {
			if s.vis.visit(nbID) {
				continue
			}
			d := ix.dist(q, v.vec(nbID))
			if len(s.res) < ef || d < s.res.top().dist {
				s.cand.push(candidate{d, nbID})
				if !liveOnly || !v.deleted(nbID) {
					s.res.push(candidate{d, nbID})
					if len(s.res) > ef {
						s.res.pop()
					}
				}
			}
		}
	}
	n := len(s.res)
	if cap(s.out) < n {
		s.out = make([]candidate, n)
	}
	out := s.out[:n]
	for i := n - 1; i >= 0; i-- {
		out[i] = s.res.pop()
	}
	s.out = out
	return out
}

// Search returns up to k approximate nearest neighbors of q. ef is the beam
// width at the bottom layer; it is raised to k if smaller.
func (ix *Index) Search(q []float32, k, ef int) ([]Result, error) {
	if len(q) != ix.cfg.Dim {
		return nil, fmt.Errorf("%w: expected %d, got %d", ErrDimension, ix.cfg.Dim, len(q))
	}
	if k <= 0 {
		return nil, nil
	}
	if ix.cfg.Metric == Cosine {
		q = slices.Clone(q)
		normalize(q)
	}
	ix.epMu.RLock()
	entry, maxL := ix.entry, ix.maxLevel
	ix.epMu.RUnlock()
	if entry < 0 {
		return nil, nil
	}
	ef = max(ef, k)
	v := ix.view()
	s := ix.getScratch()
	defer ix.putScratch(s)
	cur := uint32(entry)
	curDist := ix.dist(q, v.vec(cur))
	for l := maxL; l > 0; l-- {
		cur, curDist = ix.greedy(&v, s, q, cur, curDist, l)
	}
	found := ix.searchLayer(&v, s, q, cur, curDist, 0, ef, true)
	if len(found) > k {
		found = found[:k]
	}
	out := make([]Result, len(found))
	for i, c := range found {
		d := c.dist
		if ix.cfg.Metric == L2 {
			d = float32(math.Sqrt(float64(d)))
		}
		out[i] = Result{ID: c.id, Key: v.meta(c.id).key, Distance: d}
	}
	return out, nil
}

func (ix *Index) valid(id uint32) bool { return id < ix.n.Load() }

// Delete tombstones a node. It stays in the graph to preserve connectivity
// but is excluded from search results. It reports whether the node was live.
func (ix *Index) Delete(id uint32) bool {
	if !ix.valid(id) {
		return false
	}
	v := ix.view()
	if v.chunk(id).deleted[id&chunkMask].CompareAndSwap(false, true) {
		ix.live.Add(-1)
		return true
	}
	return false
}

// Deleted reports whether node id is tombstoned.
func (ix *Index) Deleted(id uint32) bool {
	v := ix.view()
	return v.deleted(id)
}

// Key returns the external key stored with node id.
func (ix *Index) Key(id uint32) string {
	v := ix.view()
	return v.meta(id).key
}

// Vector returns a copy of the stored vector for node id. For the cosine
// metric this is the normalized vector.
func (ix *Index) Vector(id uint32) []float32 {
	v := ix.view()
	return slices.Clone(v.vec(id))
}

// ForEach calls fn for every node in id order with its key and tombstone
// state. Nodes allocated while ForEach runs may or may not be visited.
func (ix *Index) ForEach(fn func(id uint32, key string, deleted bool)) {
	v := ix.view()
	n := ix.n.Load()
	for id := uint32(0); id < n; id++ {
		fn(id, v.meta(id).key, v.deleted(id))
	}
}
