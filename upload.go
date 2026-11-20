package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"
)

const maxFetchRequest = 4 << 20
const maxCachedFetch = 4 << 20

var uploadV2 = func() []byte {
	var reply []byte
	for _, line := range []string{"version 2\n", "ls-refs=unborn\n", "fetch=shallow\n", "object-format=sha1\n"} {
		reply = appendPacket(reply, line)
	}
	return append(reply, "0000"...)
}()

type uploadCache struct {
	head    string
	request []byte
	reply   []byte
}

func (g *gitRepo) advertiseUpload(head string, v2 bool) []byte {
	if v2 {
		return uploadV2
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	reply := appendPacket(nil, "# service=git-upload-pack\n")
	reply = append(reply, "0000"...)
	caps := "\x00side-band-64k ofs-delta shallow object-format=sha1 symref=HEAD:" + head
	if id, ok := g.refs[head]; ok {
		reply = appendPacket(reply, id.String()+" HEAD"+caps+"\n")
		caps = ""
	}
	for _, name := range g.refNames() {
		id := g.refs[name]
		reply = appendPacket(reply, id.String()+" "+name+caps+"\n")
		caps = ""
		if peeled := g.peel(id); peeled != id && strings.HasPrefix(name, "refs/tags/") {
			reply = appendPacket(reply, peeled.String()+" "+name+"^{}\n")
		}
	}
	if caps != "" {
		reply = appendPacket(reply, zeroID.String()+" capabilities^{}"+caps+"\n")
	}
	return append(reply, "0000"...)
}

func (g *gitRepo) refNames() []string {
	names := make([]string, 0, len(g.refs))
	for name := range g.refs {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func (g *gitRepo) peel(id oid) oid {
	for i := 0; i < len(g.objects); i++ {
		object := g.objects[id]
		if object == nil || object.kind != tagKind {
			break
		}
		links, err := object.edges()
		if err != nil || len(links) != 1 {
			break
		}
		id = links[0].id
	}
	return id
}

func readUploadLines(body []byte) ([]string, error) {
	var lines []string
	for len(body) >= 4 {
		size, err := packetSize(body[:4])
		if err != nil || size == 3 || size == 4 || size > maxPacket || size > len(body) {
			return nil, errors.New("invalid upload frame")
		}
		if size <= 2 {
			body = body[4:]
			continue
		}
		lines = append(lines, strings.TrimSuffix(string(body[4:size]), "\n"))
		body = body[size:]
	}
	if len(body) != 0 {
		return nil, errors.New("incomplete upload frame")
	}
	return lines, nil
}

func (g *gitRepo) listRefs(head string, lines []string) []byte {
	var prefixes []string
	for _, line := range lines {
		if prefix, ok := strings.CutPrefix(line, "ref-prefix "); ok {
			prefixes = append(prefixes, prefix)
		}
	}
	matches := func(name string) bool {
		return len(prefixes) == 0 || slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(name, p) })
	}
	var reply []byte
	if matches("HEAD") {
		if id, ok := g.refs[head]; ok {
			suffix := ""
			if slices.Contains(lines, "symrefs") {
				suffix = " symref-target:" + head
			}
			reply = appendPacket(reply, id.String()+" HEAD"+suffix+"\n")
		} else if slices.Contains(lines, "unborn") {
			reply = appendPacket(reply, "unborn HEAD symref-target:"+head+"\n")
		}
	}
	peel := slices.Contains(lines, "peel")
	for _, name := range g.refNames() {
		if !matches(name) {
			continue
		}
		id := g.refs[name]
		suffix := ""
		if peel {
			if peeled := g.peel(id); peeled != id {
				suffix = " peeled:" + peeled.String()
			}
		}
		reply = appendPacket(reply, id.String()+" "+name+suffix+"\n")
	}
	return append(reply, "0000"...)
}

type fetchQuery struct {
	wants, haves                       []oid
	shallow                            map[oid]bool
	depth                              int
	relative, done, wait, band, deepen bool
}

func fetchArgs(lines []string) (fetchQuery, error) {
	q := fetchQuery{depth: math.MaxInt - 1}
	for _, line := range lines {
		key, arg, _ := strings.Cut(line, " ")
		switch key {
		case "want", "have", "shallow":
			value, capabilities, _ := strings.Cut(arg, " ")
			id, ok := parseOID(value)
			if !ok {
				return q, errors.New("invalid fetch object ID")
			}
			switch key {
			case "want":
				q.wants = append(q.wants, id)
				q.band = q.band || slices.Contains(strings.Fields(capabilities), "side-band-64k")
			case "have":
				q.haves = append(q.haves, id)
			case "shallow":
				if q.shallow == nil {
					q.shallow = make(map[oid]bool)
				}
				q.shallow[id] = true
			}
		case "deepen":
			depth, err := strconv.Atoi(arg)
			if err != nil || depth < 1 {
				return q, errors.New("invalid fetch depth")
			}
			q.depth = min(depth, math.MaxInt-1)
			q.deepen = true
		case "deepen-relative":
			q.relative = true
		case "done":
			q.done = true
		case "wait-for-done":
			q.wait = true
		case "deepen-since", "deepen-not", "filter", "want-ref":
			return q, errors.New("unsupported fetch feature")
		}
	}
	return q, nil
}

type fetchVisit struct {
	id    oid
	depth int
}

func (g *gitRepo) reach(roots []oid, depth int, old map[oid]bool, relative bool) (map[oid]bool, map[oid]bool) {
	chosen, shallow := make(map[oid]bool), make(map[oid]bool)
	budget := make(map[oid]int)
	var todo []fetchVisit
	for _, id := range roots {
		remaining := depth
		if relative {
			remaining = math.MaxInt - 1
		}
		todo = append(todo, fetchVisit{id, remaining})
	}
	for i := 0; i < len(todo); i++ {
		v := todo[i]
		obj := g.objects[v.id]
		if obj == nil || budget[v.id] >= v.depth {
			continue
		}
		budget[v.id] = v.depth
		chosen[v.id] = true
		links, err := obj.edges()
		if err != nil {
			continue
		}
		remaining := v.depth
		if relative && old[v.id] {
			remaining = min(remaining, depth+1)
		}
		for _, link := range links {
			next := remaining
			if obj.kind == commitKind && link.kind == commitKind {
				if remaining <= 1 || (!relative && old[v.id]) {
					shallow[v.id] = true
					continue
				}
				next--
			}
			todo = append(todo, fetchVisit{link.id, next})
		}
	}
	for id := range shallow {
		links, _ := g.objects[id].edges()
		if !slices.ContainsFunc(links, func(link objectLink) bool { return link.kind == commitKind && !chosen[link.id] }) {
			delete(shallow, id)
		}
	}
	return chosen, shallow
}

func sortedIDs(ids map[oid]bool) []oid {
	keys := make([]oid, 0, len(ids))
	for id := range ids {
		keys = append(keys, id)
	}
	slices.SortFunc(keys, func(a, b oid) int { return bytes.Compare(a[:], b[:]) })
	return keys
}

func (g *gitRepo) buildPack(chosen, held map[oid]bool) ([]byte, error) {
	count := 0
	for id := range chosen {
		if !held[id] {
			count++
		}
	}
	out := binary.BigEndian.AppendUint32([]byte("PACK\x00\x00\x00\x02"), uint32(count))
	var compressed bytes.Buffer
	var writer *zlib.Writer
	for _, id := range sortedIDs(chosen) {
		if held[id] {
			continue
		}
		object := g.objects[id]
		n := len(object.data)
		first := byte(object.kind<<4) | byte(n&15)
		for n >>= 4; n != 0; n >>= 7 {
			out = append(out, first|128)
			first = byte(n & 127)
		}
		out = append(out, first)
		if object.compressed != nil {
			out = append(out, object.compressed...)
			continue
		}
		if writer == nil {
			writer, _ = zlib.NewWriterLevel(&compressed, zlib.BestSpeed)
		} else {
			compressed.Reset()
			writer.Reset(&compressed)
		}
		if _, err := writer.Write(object.data); err != nil {
			return nil, err
		}
		if err := writer.Close(); err != nil {
			return nil, err
		}
		out = append(out, compressed.Bytes()...)
	}
	sum := sha1.Sum(out)
	return append(out, sum[:]...), nil
}

func (g *gitRepo) fetch(q fetchQuery, v2 bool) ([]byte, error) {
	for _, id := range q.wants {
		if g.objects[id] == nil {
			return nil, errors.New("unknown wanted object")
		}
	}
	if len(q.wants) == 0 {
		return []byte("0000"), nil
	}
	haves := make([]oid, 0, len(q.haves))
	for _, id := range q.haves {
		if g.objects[id] != nil {
			haves = append(haves, id)
		}
	}
	var reply []byte
	if v2 && !q.done {
		reply = appendPacket(reply, "acknowledgments\n")
		if len(haves) == 0 {
			reply = appendPacket(reply, "NAK\n")
		}
		for _, id := range haves {
			reply = appendPacket(reply, "ACK "+id.String()+"\n")
		}
		if len(haves) == 0 || q.wait {
			return append(reply, "0000"...), nil
		}
		reply = appendPacket(reply, "ready\n")
		reply = append(reply, "0001"...)
	}
	relative := q.relative && len(q.shallow) > 0 && q.deepen
	boundaries := q.shallow
	if !relative && q.deepen {
		boundaries = nil
	}
	chosen, shallow := g.reach(q.wants, q.depth, boundaries, relative)
	held, _ := g.reach(haves, math.MaxInt-1, q.shallow, false)
	var updates []byte
	for _, id := range sortedIDs(shallow) {
		if !q.shallow[id] {
			updates = appendPacket(updates, "shallow "+id.String()+"\n")
		}
	}
	for _, id := range sortedIDs(q.shallow) {
		if chosen[id] && !shallow[id] {
			updates = appendPacket(updates, "unshallow "+id.String()+"\n")
		}
	}
	if v2 {
		if len(updates) > 0 {
			reply = appendPacket(reply, "shallow-info\n")
			reply = append(reply, updates...)
			reply = append(reply, "0001"...)
		}
		reply = appendPacket(reply, "packfile\n")
	} else {
		reply = append(reply, updates...)
		if q.deepen {
			reply = append(reply, "0000"...)
		}
		if !q.done {
			return reply, nil
		}
		reply = appendPacket(reply, "NAK\n")
	}
	pack, err := g.buildPack(chosen, held)
	if err != nil {
		return nil, err
	}
	if v2 || q.band {
		return appendSideband(reply, pack), nil
	}
	return append(reply, pack...), nil
}

// Request buffers may return to the HTTP pool after upload. A published
// cache entry owns its key and its immutable response.
func (g *gitRepo) cachedUpload(body []byte, head string) []byte {
	for _, cache := range [...]*uploadCache{g.fetchCache.Load(), g.refsCache.Load()} {
		if cache != nil && cache.head == head && bytes.Equal(cache.request, body) {
			return cache.reply
		}
	}
	return nil
}

func (g *gitRepo) upload(body []byte, head string, v2 bool) ([]byte, error) {
	cacheable := v2 && len(body) <= 4096
	if cacheable {
		if reply := g.cachedUpload(body, head); reply != nil {
			return reply, nil
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if cacheable {
		if reply := g.cachedUpload(body, head); reply != nil {
			return reply, nil
		}
	}
	lines, err := readUploadLines(body)
	if err != nil {
		return nil, err
	}
	if v2 {
		switch {
		case slices.Contains(lines, "command=ls-refs"):
			reply := g.listRefs(head, lines)
			if cacheable && len(reply) <= maxCachedFetch {
				g.refsCache.Store(&uploadCache{head, bytes.Clone(body), reply})
			}
			return reply, nil
		case !slices.Contains(lines, "command=fetch"):
			return nil, errors.New("unsupported upload command")
		}
	}
	q, err := fetchArgs(lines)
	if err != nil {
		return nil, err
	}
	reply, err := g.fetch(q, v2)
	if err == nil && cacheable && q.done && len(q.haves) == 0 && len(q.shallow) == 0 && len(reply) <= maxCachedFetch {
		g.fetchCache.Store(&uploadCache{head, bytes.Clone(body), reply})
	}
	return reply, err
}
