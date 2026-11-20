package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"testing"
)

func appendEntry(pack []byte, kind objectKind, base, data []byte) []byte {
	n := len(data)
	first := byte(kind<<4) | byte(n&15)
	for n >>= 4; n != 0; n >>= 7 {
		pack = append(pack, first|128)
		first = byte(n & 127)
	}
	pack = append(append(pack, first), base...)
	var compressed bytes.Buffer
	z := zlib.NewWriter(&compressed)
	_, _ = z.Write(data)
	_ = z.Close()
	return append(pack, compressed.Bytes()...)
}
func packHeader(count uint32) []byte {
	header := append([]byte("PACK"), 0, 0, 0, 2)
	return binary.BigEndian.AppendUint32(header, count)
}
func finishPack(pack []byte) []byte {
	sum := sha1.Sum(pack)
	return append(pack, sum[:]...)
}
func singlePack(kind objectKind, data []byte) []byte {
	return finishPack(appendEntry(packHeader(1), kind, nil, data))
}
func TestUnpackDeltaChains(t *testing.T) {
	base := &gitObject{kind: blobKind, data: []byte("abc")}
	baseID := base.id()
	middle := &gitObject{kind: blobKind, data: []byte("abc!")}
	middleID := middle.id()
	end := &gitObject{kind: blobKind, data: []byte("abc!?")}
	endID := end.id()
	pack := packHeader(3)
	// REF_DELTA first, with the base resolved later by an OFS_DELTA.
	pack = appendEntry(pack, refDelta, middleID[:], []byte{4, 5, 0x90, 4, 1, '?'})
	original := len(pack)
	pack = appendEntry(pack, blobKind, nil, base.data)
	pack = appendEntry(pack, offsetDelta, []byte{byte(len(pack) - original)}, []byte{3, 4, 0x90, 3, 1, '!'})
	got, err := unpack(finishPack(pack), nil)
	if err != nil || got[endID] == nil || !bytes.Equal(got[endID].data, end.data) {
		t.Fatalf("delta chain: %v", err)
	}
	thin := finishPack(appendEntry(packHeader(1), refDelta, baseID[:], []byte{3, 4, 0x90, 3, 1, '!'}))
	got, err = unpack(thin, map[oid]*gitObject{baseID: base})
	if err != nil || got[middleID] == nil || string(got[middleID].data) != "abc!" {
		t.Fatalf("thin pack: %v", err)
	}
	if _, err := unpack(thin, nil); err == nil {
		t.Fatal("accepted thin pack without its base")
	}
	if _, err := applyDelta(base.data, []byte{3, 5, 0x91, 2, 5}, 100); err == nil {
		t.Fatal("accepted delta that copies past the base")
	}
}
func TestRejectInvalidPacks(t *testing.T) {
	fixture := singlePack(blobKind, []byte("hello"))
	fixture[len(fixture)-1] ^= 1
	if _, err := unpack(fixture, nil); err == nil {
		t.Fatal("accepted invalid trailer")
	}
	payload := fixture[:len(fixture)-sha1.Size]
	payload[len(payload)-1] ^= 1
	if _, err := unpack(finishPack(payload), nil); err == nil {
		t.Fatal("accepted invalid zlib checksum")
	}
	malformed := []byte{0x31, 0x78, 0x9c, 0x03, 0, 0, 0, 0, 1}
	if _, err := unpack(finishPack(append(packHeader(1), malformed...)), nil); err == nil {
		t.Fatal("accepted size mismatch")
	}
}
func commandLine(old, next oid, ref, suffix string) string {
	return old.String() + " " + next.String() + " " + ref + suffix + "\n"
}

func TestUploadCacheOwnsRequestAndRefreshesRefs(t *testing.T) {
	var repo gitRepo
	const branch = "refs/heads/main"
	before := oid{1}
	after := oid{2}
	repo.setRef(branch, before)

	request := append(appendPacket(nil, "command=ls-refs\n"), "00010000"...)
	borrowed := bytes.Clone(request)
	oldReply, err := repo.upload(borrowed, branch, true)
	if err != nil || !bytes.Contains(oldReply, []byte(before.String())) {
		t.Fatalf("initial advertisement: %q, %v", oldReply, err)
	}
	clear(borrowed)
	if cached := repo.cachedUpload(request, branch); !bytes.Equal(cached, oldReply) {
		t.Fatal("the cache key changed after its request buffer was cleared")
	}
	if cached := repo.cachedUpload(request, "refs/heads/alternate"); cached != nil {
		t.Fatal("the cache hit with a different HEAD")
	}

	repo.setRef(branch, after)
	reply, err := repo.upload(request, branch, true)
	if err != nil || !bytes.Contains(reply, []byte(after.String())) || bytes.Contains(reply, []byte(before.String())) {
		t.Fatalf("updated advertisement: %q, %v", reply, err)
	}
	if !bytes.Contains(oldReply, []byte(before.String())) {
		t.Fatal("ref changes modified an earlier response")
	}
}

func TestReceiveRefs(t *testing.T) {
	tree := &gitObject{kind: treeKind}
	treeID := tree.id()
	commit := &gitObject{kind: commitKind, data: []byte("tree " + treeID.String() + "\n\nmessage\n")}
	tip := commit.id()
	pack := appendEntry(packHeader(2), treeKind, nil, tree.data)
	pack = finishPack(appendEntry(pack, commitKind, nil, commit.data))
	var g gitRepo
	const main = "refs/heads/main"
	const side = "refs/heads/side"
	cmd := appendPacket(nil, commandLine(zeroID, tip, main, "\x00report-status side-band-64k"))
	cmd = appendPacket(cmd, commandLine(treeID, tip, side, ""))
	report, err := g.receive(append(append(cmd, "0000"...), pack...))
	if err != nil || !bytes.Contains(report, []byte("ok "+main)) || !bytes.Contains(report, []byte("ng "+side+" stale info")) ||
		g.refs[main] != tip || len(g.objects) != 2 {
		t.Fatalf("unexpected mixed push: %s %v", report, err)
	}
	report, err = g.receive(append(appendPacket(nil, commandLine(zeroID, treeID, side, "\x00report-status")), "0000PACK"...))
	if err != nil || !bytes.Contains(report, []byte("unpack invalid pack")) || len(g.refs) != 1 {
		t.Fatalf("invalid pack: %s %v", report, err)
	}
	other := &gitObject{kind: commitKind, data: []byte("tree " + treeID.String() + "\n\nother\n")}
	report, err = g.receive(append(append(appendPacket(nil, commandLine(tip, other.id(), main, "\x00report-status")), "0000"...), singlePack(commitKind, other.data)...))
	if err != nil || !bytes.Contains(report, []byte("non-fast-forward")) || g.objects[other.id()] != nil {
		t.Fatalf("ref moved or rejected object was saved: %s %v", report, err)
	}
	report, err = g.receive(append(appendPacket(nil, commandLine(tip, zeroID, main, "\x00report-status")), "0000"...))
	if err != nil || !bytes.Contains(report, []byte("ok "+main)) || len(g.refs) != 0 || len(g.objects) != 2 {
		t.Fatalf("unexpected deletion: %s %v", report, err)
	}
}
