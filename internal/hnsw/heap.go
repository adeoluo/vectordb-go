package hnsw

// candidate pairs a node id with its distance to the current query.
type candidate struct {
	dist float32
	id   uint32
}

// minHeap and maxHeap are specialized binary heaps. container/heap would
// work, but its interface calls and boxing cost noticeably in the inner
// search loop, so the sift operations are written out directly.
type minHeap []candidate

func (h *minHeap) push(c candidate) {
	*h = append(*h, c)
	s := *h
	i := len(s) - 1
	for i > 0 {
		p := (i - 1) / 2
		if s[p].dist <= s[i].dist {
			break
		}
		s[p], s[i] = s[i], s[p]
		i = p
	}
}

func (h *minHeap) pop() candidate {
	s := *h
	top := s[0]
	n := len(s) - 1
	s[0] = s[n]
	s = s[:n]
	i := 0
	for {
		l := 2*i + 1
		if l >= n {
			break
		}
		m := l
		if r := l + 1; r < n && s[r].dist < s[l].dist {
			m = r
		}
		if s[i].dist <= s[m].dist {
			break
		}
		s[i], s[m] = s[m], s[i]
		i = m
	}
	*h = s
	return top
}

type maxHeap []candidate

func (h *maxHeap) push(c candidate) {
	*h = append(*h, c)
	s := *h
	i := len(s) - 1
	for i > 0 {
		p := (i - 1) / 2
		if s[p].dist >= s[i].dist {
			break
		}
		s[p], s[i] = s[i], s[p]
		i = p
	}
}

func (h *maxHeap) pop() candidate {
	s := *h
	top := s[0]
	n := len(s) - 1
	s[0] = s[n]
	s = s[:n]
	i := 0
	for {
		l := 2*i + 1
		if l >= n {
			break
		}
		m := l
		if r := l + 1; r < n && s[r].dist > s[l].dist {
			m = r
		}
		if s[i].dist >= s[m].dist {
			break
		}
		s[i], s[m] = s[m], s[i]
		i = m
	}
	*h = s
	return top
}

func (h maxHeap) top() candidate { return h[0] }

// visited is a generation-stamped visited set. Resetting it is O(1): the
// generation counter is bumped instead of clearing the array, and the
// array is cleared only when the 16-bit counter wraps. 16-bit marks halve
// the cache footprint compared with 32-bit ones.
type visited struct {
	marks []uint16
	gen   uint16
}

func (v *visited) reset(size int) {
	if len(v.marks) < size {
		v.marks = make([]uint16, size+size/4+64)
		v.gen = 0
	}
	v.gen++
	if v.gen == 0 {
		clear(v.marks)
		v.gen = 1
	}
}

// visit marks id and reports whether it had already been visited.
func (v *visited) visit(id uint32) bool {
	if int(id) >= len(v.marks) {
		grown := make([]uint16, int(id)*2+64)
		copy(grown, v.marks)
		v.marks = grown
	}
	if v.marks[id] == v.gen {
		return true
	}
	v.marks[id] = v.gen
	return false
}

// scratch holds per-operation buffers, recycled through a sync.Pool so
// steady-state searches do not allocate.
type scratch struct {
	vis   visited
	cand  minHeap
	res   maxHeap
	nbuf  []uint32
	out   []candidate
	sel   []candidate
	prune []candidate
	ids   []uint32
}
