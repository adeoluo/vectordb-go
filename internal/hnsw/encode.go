package hnsw

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
)

const (
	encMagic   = 0x57534e48 // "HNSW" little endian
	encVersion = 1
)

type encoder struct {
	w   *bufio.Writer
	buf [8]byte
	err error
}

func (e *encoder) u8(v uint8) {
	if e.err == nil {
		e.err = e.w.WriteByte(v)
	}
}

func (e *encoder) u32(v uint32) {
	if e.err == nil {
		binary.LittleEndian.PutUint32(e.buf[:4], v)
		_, e.err = e.w.Write(e.buf[:4])
	}
}

func (e *encoder) u64(v uint64) {
	if e.err == nil {
		binary.LittleEndian.PutUint64(e.buf[:8], v)
		_, e.err = e.w.Write(e.buf[:8])
	}
}

func (e *encoder) bytes(b []byte) {
	if e.err == nil {
		_, e.err = e.w.Write(b)
	}
}

// WriteTo serializes the full graph: configuration, entry point, and every
// node with its key, level, tombstone flag, vector and adjacency lists.
//
// The caller must ensure no Allocate or Link calls run concurrently, so the
// snapshot is a consistent cut. Concurrent searches are fine.
func (ix *Index) WriteTo(w io.Writer) (int64, error) {
	cw := &countingWriter{w: w}
	e := &encoder{w: bufio.NewWriterSize(cw, 1<<20)}
	e.u32(encMagic)
	e.u32(encVersion)
	e.u32(uint32(ix.cfg.Dim))
	e.u32(uint32(ix.cfg.M))
	e.u32(uint32(ix.cfg.EfConstruction))
	e.u8(uint8(ix.cfg.Metric))
	e.u64(uint64(ix.cfg.Seed))
	ix.epMu.RLock()
	e.u64(uint64(ix.entry))
	e.u32(uint32(ix.maxLevel))
	ix.epMu.RUnlock()
	v := ix.view()
	n := ix.n.Load()
	e.u32(n)
	vbuf := make([]byte, 4*ix.cfg.Dim)
	var links []uint32
	for id := uint32(0); id < n; id++ {
		m := v.meta(id)
		e.u32(uint32(len(m.key)))
		e.bytes([]byte(m.key))
		e.u32(uint32(m.level))
		if v.deleted(id) {
			e.u8(1)
		} else {
			e.u8(0)
		}
		for i, x := range v.vec(id) {
			binary.LittleEndian.PutUint32(vbuf[4*i:], math.Float32bits(x))
		}
		e.bytes(vbuf)
		for l := 0; l <= m.level; l++ {
			links = v.links(id, l, links)
			e.u32(uint32(len(links)))
			for _, x := range links {
				e.u32(x)
			}
		}
	}
	if e.err == nil {
		e.err = e.w.Flush()
	}
	return cw.n, e.err
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

type decoder struct {
	r   *bufio.Reader
	buf [8]byte
	err error
}

func (d *decoder) u8() uint8 {
	if d.err != nil {
		return 0
	}
	var b byte
	b, d.err = d.r.ReadByte()
	return b
}

func (d *decoder) u32() uint32 {
	if d.err != nil {
		return 0
	}
	if _, d.err = io.ReadFull(d.r, d.buf[:4]); d.err != nil {
		return 0
	}
	return binary.LittleEndian.Uint32(d.buf[:4])
}

func (d *decoder) u64() uint64 {
	if d.err != nil {
		return 0
	}
	if _, d.err = io.ReadFull(d.r, d.buf[:8]); d.err != nil {
		return 0
	}
	return binary.LittleEndian.Uint64(d.buf[:8])
}

// ErrBadEncoding is returned when a serialized index fails validation.
var ErrBadEncoding = errors.New("invalid index encoding")

// Read deserializes an index written by WriteTo. Every adjacency id is
// bounds checked, so a damaged file cannot produce an index that panics.
func Read(r io.Reader) (*Index, error) {
	d := &decoder{r: bufio.NewReaderSize(r, 1<<20)}
	if d.u32() != encMagic || d.err != nil {
		return nil, fmt.Errorf("%w: bad magic", ErrBadEncoding)
	}
	if v := d.u32(); v != encVersion {
		return nil, fmt.Errorf("%w: unsupported version %d", ErrBadEncoding, v)
	}
	cfg := Config{
		Dim:            int(d.u32()),
		M:              int(d.u32()),
		EfConstruction: int(d.u32()),
		Metric:         Metric(d.u8()),
		Seed:           int64(d.u64()),
	}
	entry := int64(d.u64())
	maxLevel := int(d.u32())
	count := d.u32()
	if d.err != nil {
		return nil, fmt.Errorf("%w: header: %v", ErrBadEncoding, d.err)
	}
	ix, err := New(cfg)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadEncoding, err)
	}
	vbuf := make([]byte, 4*cfg.Dim)
	vec := make([]float32, cfg.Dim)
	var live int64
	var links []uint32
	v := ix.view()
	for i := uint32(0); i < count; i++ {
		klen := d.u32()
		if klen > 1<<16 {
			return nil, fmt.Errorf("%w: key length %d", ErrBadEncoding, klen)
		}
		kb := make([]byte, klen)
		if d.err == nil {
			_, d.err = io.ReadFull(d.r, kb)
		}
		level := d.u32()
		if level > 64 {
			return nil, fmt.Errorf("%w: level %d", ErrBadEncoding, level)
		}
		del := d.u8()
		if d.err == nil {
			_, d.err = io.ReadFull(d.r, vbuf)
		}
		if d.err != nil {
			return nil, fmt.Errorf("%w: node %d: %v", ErrBadEncoding, i, d.err)
		}
		for j := range vec {
			vec[j] = math.Float32frombits(binary.LittleEndian.Uint32(vbuf[4*j:]))
		}
		// Stored vectors are already normalized for the cosine metric.
		id, err := ix.appendLocked(string(kb), int(level), vec, false)
		if err != nil {
			return nil, err
		}
		v.chunks = *ix.chunks.Load()
		ch, off := v.chunk(id), id&chunkMask
		for l := 0; l <= int(level); l++ {
			cnt := d.u32()
			if int(cnt) > ix.maxM(l) {
				return nil, fmt.Errorf("%w: node %d level %d has %d links", ErrBadEncoding, i, l, cnt)
			}
			links = links[:0]
			for range cnt {
				x := d.u32()
				if x >= count {
					return nil, fmt.Errorf("%w: link out of range", ErrBadEncoding)
				}
				links = append(links, x)
			}
			v.setLinksLocked(ch, off, l, links)
		}
		if d.err != nil {
			return nil, fmt.Errorf("%w: node %d: %v", ErrBadEncoding, i, d.err)
		}
		if del == 1 {
			ch.deleted[off].Store(true)
		} else {
			live++
		}
	}
	if entry >= int64(count) || (count > 0 && entry < 0) {
		return nil, fmt.Errorf("%w: entry point %d", ErrBadEncoding, entry)
	}
	// A link at layer l must point at a node that exists on layer l.
	v.chunks = *ix.chunks.Load()
	for id := uint32(0); id < count; id++ {
		for l := 1; l <= v.meta(id).level; l++ {
			for _, x := range v.links(id, l, links) {
				if v.meta(x).level < l {
					return nil, fmt.Errorf("%w: node %d links to node %d above its level", ErrBadEncoding, id, x)
				}
			}
		}
	}
	if count > 0 && v.meta(uint32(entry)).level != maxLevel {
		return nil, fmt.Errorf("%w: entry point level mismatch", ErrBadEncoding)
	}
	ix.entry = entry
	ix.maxLevel = maxLevel
	ix.live.Store(live)
	// Continue the level sequence from a different stream than a fresh
	// index would use, so restored and new nodes do not share levels.
	ix.rng = rand.New(rand.NewSource(cfg.Seed + int64(count)))
	return ix, nil
}
