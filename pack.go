package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"strconv"
	"sync"
)

const (
	maxObjectBytes  = 32 << 20
	maxPackExpanded = 128 << 20
	maxPackObjects  = 50000
)

type oid [sha1.Size]byte

func parseOID(s string) (result oid, ok bool) {
	if len(s) != hex.EncodedLen(len(result)) {
		return result, false
	}
	_, err := hex.Decode(result[:], []byte(s))
	return result, err == nil
}

func (id oid) String() string { return hex.EncodeToString(id[:]) }

type objectKind uint8

const (
	commitKind objectKind = iota + 1
	treeKind
	blobKind
	tagKind
	offsetDelta objectKind = 6
	refDelta    objectKind = 7
)

var kindNames = [...]string{"", "commit", "tree", "blob", "tag"}

type gitObject struct {
	kind         objectKind
	data         []byte
	compressed   []byte
	links        []objectLink
	linksChecked bool
	linksErr     error
}

type objectLink struct {
	id   oid
	kind objectKind
}

func (obj *gitObject) id() (id oid) {
	var suffix [32]byte
	header := append(suffix[:0], kindNames[obj.kind]...)
	header = append(header, ' ')
	header = strconv.AppendInt(header, int64(len(obj.data)), 10)
	header = append(header, 0)
	digest := sha1.New()
	_, _ = digest.Write(header)
	_, _ = digest.Write(obj.data)
	digest.Sum(id[:0])
	return id
}

var inflaters sync.Pool

type packReader struct {
	stream   *bytes.Reader
	inflater io.ReadCloser
	used     int
}

func (p *packReader) close() {
	if p.inflater != nil {
		// Reset drops the compressed input reference before pooling.
		if p.inflater.(zlib.Resetter).Reset(bytes.NewReader(emptyZlib), nil) == nil {
			inflaters.Put(p.inflater)
		}
	}
}

var emptyZlib = []byte{0x78, 0x9c, 0x03, 0x00, 0x00, 0x00, 0x00, 0x01}

func (p *packReader) inflate(size int) ([]byte, error) {
	if size > maxObjectBytes || size > maxPackExpanded-p.used {
		return nil, errors.New("pack exceeds inflated size budget")
	}
	p.used += size
	var err error
	if p.inflater == nil {
		if pooled, ok := inflaters.Get().(io.ReadCloser); ok {
			p.inflater = pooled
			err = pooled.(zlib.Resetter).Reset(p.stream, nil)
		} else {
			p.inflater, err = zlib.NewReader(p.stream)
		}
	} else {
		err = p.inflater.(zlib.Resetter).Reset(p.stream, nil)
	}
	if err != nil {
		return nil, err
	}
	result := make([]byte, size+1)
	n, err := io.ReadFull(p.inflater, result)
	if n != size || (err != io.EOF && err != io.ErrUnexpectedEOF) {
		return nil, errors.New("pack object size or checksum mismatch")
	}
	return result[:size:size], nil
}

func objectHeader(stream *bytes.Reader) (objectKind, int, error) {
	first, err := stream.ReadByte()
	if err != nil {
		return 0, 0, err
	}
	kind, size := objectKind(first>>4&7), int(first&15)
	for shift := 4; first&128 != 0; shift += 7 {
		if shift > 25 {
			return 0, 0, errors.New("invalid packed size")
		}
		first, err = stream.ReadByte()
		if err != nil {
			return 0, 0, err
		}
		size |= int(first&127) << shift
	}
	return kind, size, nil
}

func deltaOffset(stream *bytes.Reader, entry int) (int, error) {
	n, err := stream.ReadByte()
	distance := int(n & 127)
	for err == nil && n&128 != 0 {
		if distance >= (entry-12)/128+1 {
			return 0, errors.New("invalid offset delta")
		}
		n, err = stream.ReadByte()
		distance = (distance+1)*128 + int(n&127)
	}
	if err != nil || distance == 0 || distance > entry-12 {
		return 0, errors.New("invalid offset delta")
	}
	return entry - distance, nil
}

type packEntry struct {
	at         int
	baseOffset int
	baseID     oid
	kind       objectKind
	data       []byte
	object     *gitObject
}

func unpack(pack []byte, existing map[oid]*gitObject) (map[oid]*gitObject, error) {
	if len(pack) < 12+sha1.Size || string(pack[:4]) != "PACK" {
		return nil, errors.New("invalid pack header")
	}
	version := binary.BigEndian.Uint32(pack[4:8])
	count := binary.BigEndian.Uint32(pack[8:12])
	if (version != 2 && version != 3) || count > maxPackObjects {
		return nil, errors.New("unsupported pack version or object count")
	}
	end := len(pack) - sha1.Size
	checksum := sha1.Sum(pack[:end])
	if !bytes.Equal(checksum[:], pack[end:]) {
		return nil, errors.New("pack checksum mismatch")
	}
	p := packReader{stream: bytes.NewReader(pack[12:end])}
	defer p.close()
	entries := make([]packEntry, count)
	staged := make(map[oid]*gitObject, count)
	byOffset := make(map[int]*packEntry, count)
	hasDelta := false
	for i := range entries {
		entry := &entries[i]
		entry.at = end - p.stream.Len()
		kind, size, err := objectHeader(p.stream)
		if err != nil {
			return nil, err
		}
		entry.kind = kind
		switch kind {
		case commitKind, treeKind, blobKind, tagKind:
		case offsetDelta:
			entry.baseOffset, err = deltaOffset(p.stream, entry.at)
			if err == nil && byOffset[entry.baseOffset] == nil {
				return nil, errors.New("offset delta base is not an object")
			}
			hasDelta = true
		case refDelta:
			_, err = io.ReadFull(p.stream, entry.baseID[:])
			hasDelta = true
		default:
			return nil, errors.New("invalid pack object type")
		}
		if err != nil {
			return nil, err
		}
		compressedStart := end - p.stream.Len()
		entry.data, err = p.inflate(size)
		if err != nil {
			return nil, err
		}
		byOffset[entry.at] = entry
		if kind < offsetDelta {
			var compressed []byte
			frame := pack[compressedStart : end-p.stream.Len()]
			// Keep independent storage for the small frames most likely to be reused.
			if len(frame) <= 4096 {
				compressed = bytes.Clone(frame)
			}
			entry.object = &gitObject{kind: kind, data: entry.data, compressed: compressed}
			staged[entry.object.id()] = entry.object
		}
	}
	if p.stream.Len() != 0 {
		return nil, errors.New("unexpected trailing pack bytes")
	}
	if !hasDelta {
		return staged, nil
	}
	return resolveDeltas(entries, byOffset, staged, existing, p.used)
}

type deltaWork struct {
	entry *packEntry
	base  *gitObject
}

func resolveDeltas(entries []packEntry, offsets map[int]*packEntry, staged, existing map[oid]*gitObject, used int) (map[oid]*gitObject, error) {
	waitOffset := make(map[int][]*packEntry)
	waitID := make(map[oid][]*packEntry)
	var ready []deltaWork
	unresolved := 0
	for i := range entries {
		entry := &entries[i]
		if entry.object != nil {
			continue
		}
		unresolved++
		if entry.kind == offsetDelta {
			if base := offsets[entry.baseOffset].object; base != nil {
				ready = append(ready, deltaWork{entry, base})
			} else {
				waitOffset[entry.baseOffset] = append(waitOffset[entry.baseOffset], entry)
			}
			continue
		}
		base := staged[entry.baseID]
		if base == nil {
			base = existing[entry.baseID]
		}
		if base == nil {
			waitID[entry.baseID] = append(waitID[entry.baseID], entry)
		} else {
			ready = append(ready, deltaWork{entry, base})
		}
	}
	for i := 0; i < len(ready); i++ {
		job := ready[i]
		data, err := applyDelta(job.base.data, job.entry.data, maxPackExpanded-used)
		if err != nil {
			return nil, err
		}
		used += len(data)
		object := &gitObject{kind: job.base.kind, data: data}
		id := object.id()
		staged[id] = object
		unresolved--
		for _, child := range waitOffset[job.entry.at] {
			ready = append(ready, deltaWork{child, object})
		}
		for _, child := range waitID[id] {
			ready = append(ready, deltaWork{child, object})
		}
		delete(waitOffset, job.entry.at)
		delete(waitID, id)
	}
	if unresolved != 0 {
		return nil, errors.New("missing or cyclic delta base")
	}
	return staged, nil
}

func deltaSize(raw []byte) (int, []byte, bool) {
	value := 0
	for i, b := range raw {
		if i > 4 {
			break
		}
		value |= int(b&127) << (7 * i)
		if b&128 == 0 && value <= maxObjectBytes {
			return value, raw[i+1:], true
		}
	}
	return 0, nil, false
}

func applyDelta(base, raw []byte, budget int) ([]byte, error) {
	baseSize, raw, ok := deltaSize(raw)
	if !ok || baseSize != len(base) {
		return nil, errors.New("delta base size mismatch")
	}
	size, raw, ok := deltaSize(raw)
	if !ok || size > budget {
		return nil, errors.New("invalid delta output size")
	}
	result := make([]byte, 0, size)
	for len(raw) > 0 {
		command := raw[0]
		raw = raw[1:]
		if command&128 == 0 {
			n := int(command)
			if n == 0 || n > len(raw) || n > size-len(result) {
				return nil, errors.New("invalid delta insert")
			}
			result = append(result, raw[:n]...)
			raw = raw[n:]
			continue
		}
		offset, n := 0, 0
		for bit := 0; bit < 7; bit++ {
			if command&(1<<bit) == 0 {
				continue
			}
			if len(raw) == 0 {
				return nil, errors.New("incomplete delta copy")
			}
			if bit < 4 {
				offset |= int(raw[0]) << (8 * bit)
			} else {
				n |= int(raw[0]) << (8 * (bit - 4))
			}
			raw = raw[1:]
		}
		if n == 0 {
			n = 65536
		}
		if offset > len(base) || n > len(base)-offset || n > size-len(result) {
			return nil, errors.New("invalid delta copy")
		}
		result = append(result, base[offset:offset+n]...)
	}
	if len(result) != size {
		return nil, errors.New("incomplete delta output")
	}
	return result, nil
}
