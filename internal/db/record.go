package db

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

const (
	opUpsert byte = 1
	opDelete byte = 2
)

// Item is one vector to upsert.
type Item struct {
	ID     string
	Vector []float32
}

// MaxIDLength bounds external vector ids.
const MaxIDLength = 512

var errBadRecord = errors.New("malformed log record")

func encodeUpsert(items []Item, dim int) []byte {
	size := 5
	for _, it := range items {
		size += 2 + len(it.ID) + 4*dim
	}
	buf := make([]byte, 0, size)
	buf = append(buf, opUpsert)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(items)))
	for _, it := range items {
		buf = binary.LittleEndian.AppendUint16(buf, uint16(len(it.ID)))
		buf = append(buf, it.ID...)
		for _, x := range it.Vector {
			buf = binary.LittleEndian.AppendUint32(buf, math.Float32bits(x))
		}
	}
	return buf
}

func encodeDelete(ids []string) []byte {
	buf := []byte{opDelete}
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(ids)))
	for _, id := range ids {
		buf = binary.LittleEndian.AppendUint16(buf, uint16(len(id)))
		buf = append(buf, id...)
	}
	return buf
}

// decodeRecord parses a log payload into either upsert items or delete ids.
func decodeRecord(b []byte, dim int) (op byte, items []Item, ids []string, err error) {
	if len(b) < 5 {
		return 0, nil, nil, errBadRecord
	}
	op = b[0]
	n := int(binary.LittleEndian.Uint32(b[1:5]))
	b = b[5:]
	readID := func() (string, bool) {
		if len(b) < 2 {
			return "", false
		}
		l := int(binary.LittleEndian.Uint16(b))
		if len(b) < 2+l {
			return "", false
		}
		id := string(b[2 : 2+l])
		b = b[2+l:]
		return id, true
	}
	switch op {
	case opUpsert:
		if n > len(b) {
			return 0, nil, nil, errBadRecord
		}
		items = make([]Item, 0, n)
		for range n {
			id, ok := readID()
			if !ok || len(b) < 4*dim {
				return 0, nil, nil, errBadRecord
			}
			v := make([]float32, dim)
			for j := range v {
				v[j] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*j:]))
			}
			b = b[4*dim:]
			items = append(items, Item{ID: id, Vector: v})
		}
	case opDelete:
		if n > len(b) {
			return 0, nil, nil, errBadRecord
		}
		ids = make([]string, 0, n)
		for range n {
			id, ok := readID()
			if !ok {
				return 0, nil, nil, errBadRecord
			}
			ids = append(ids, id)
		}
	default:
		return 0, nil, nil, fmt.Errorf("%w: unknown op %d", errBadRecord, op)
	}
	if len(b) != 0 {
		return 0, nil, nil, fmt.Errorf("%w: %d trailing bytes", errBadRecord, len(b))
	}
	return op, items, ids, nil
}
