package main

import (
	"bytes"
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
)

const (
	maxPackBody     = 64 << 20
	maxRepoBytes    = 192 << 20
	maxPushCommands = 4096
	maxPacket       = 65520
)

var zeroID oid

var smallGitBodies = sync.Pool{New: func() any { return new([4096]byte) }}

func readGitBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, func(), error) {
	body := http.MaxBytesReader(w, r.Body, limit)
	if r.ContentLength > 0 && r.ContentLength <= 4096 {
		buffer := smallGitBodies.Get().(*[4096]byte)
		release := func() { smallGitBodies.Put(buffer) }
		data := buffer[:int(r.ContentLength)]
		if _, err := io.ReadFull(body, data); err != nil {
			return nil, release, err
		}
		// Preserve the request parser's EOF transition on keep-alive.
		var extra [1]byte
		n, err := body.Read(extra[:])
		if n != 0 || err != io.EOF {
			return nil, release, errors.New("invalid Git body length")
		}
		return data, release, nil
	}
	data, err := io.ReadAll(body)
	return data, func() {}, err
}

type gitRepo struct {
	mu                    sync.Mutex
	refs                  map[string]oid
	objects               map[oid]*gitObject
	bytes                 int
	advertisement         []byte
	refsCache, fetchCache uploadCache
}

func (g *gitRepo) advertise() []byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.advertisement != nil {
		return g.advertisement
	}
	names := make([]string, 0, len(g.refs))
	for name := range g.refs {
		names = append(names, name)
	}
	slices.Sort(names)
	if len(names) == 0 {
		names = append(names, "capabilities^{}")
	}
	out := appendPacket(nil, "# service=git-receive-pack\n")
	out = append(out, "0000"...)
	for i, name := range names {
		suffix := "\n"
		if i == 0 {
			suffix = "\x00report-status report-status-v2 delete-refs side-band-64k ofs-delta object-format=sha1\n"
		}
		out = appendPacket(out, g.refs[name].String()+" "+name+suffix)
	}
	g.advertisement = append(out, "0000"...)
	return g.advertisement
}

func appendPacketHeader(out []byte, n int) []byte {
	const hex = "0123456789abcdef"
	return append(out, hex[n>>12], hex[n>>8&15], hex[n>>4&15], hex[n&15])
}

func appendPacket(out []byte, payload string) []byte {
	return append(appendPacketHeader(out, len(payload)+4), payload...)
}

func appendSideband(out, data []byte) []byte {
	for len(data) > 0 {
		n := min(len(data), maxPacket-5)
		out = append(appendPacketHeader(out, n+5), 1)
		out = append(out, data[:n]...)
		data = data[n:]
	}
	return append(out, "0000"...)
}

func frameReport(data []byte) []byte {
	return appendSideband(make([]byte, 0, len(data)+9), data)
}

func validRef(name string) bool {
	if len(name) > 255 || (!strings.HasPrefix(name, "refs/heads/") && !strings.HasPrefix(name, "refs/tags/")) ||
		strings.ContainsAny(name, "~^:?*[\\") || strings.Contains(name, "..") || strings.Contains(name, "@{") || strings.HasSuffix(name, ".") {
		return false
	}
	for i := 0; i < len(name); i++ {
		if name[i] <= 32 || name[i] == 127 {
			return false
		}
	}
	for part := range strings.SplitSeq(name, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

type refCommand struct {
	old, next oid
	ref       string
}

type reportOptions struct {
	report, sideband bool
}

func packetSize(raw []byte) (int, error) {
	n := 0
	for _, b := range raw {
		n *= 16
		switch {
		case b >= '0' && b <= '9':
			n += int(b - '0')
		case b >= 'a' && b <= 'f':
			n += int(b - 'a' + 10)
		case b >= 'A' && b <= 'F':
			n += int(b - 'A' + 10)
		default:
			return 0, errors.New("invalid pkt-line")
		}
	}
	return n, nil
}

func parsePush(data []byte) ([]refCommand, reportOptions, []byte, error) {
	var commands []refCommand
	var opts reportOptions
	seen := make(map[string]bool)
	for len(data) >= 4 {
		n, err := packetSize(data[:4])
		if err != nil {
			return nil, opts, nil, err
		}

		if n == 0 {
			return commands, opts, data[4:], nil
		}
		if n <= 4 || n > maxPacket || n > len(data) || len(commands) == maxPushCommands {
			break
		}
		line := strings.TrimSuffix(string(data[4:n]), "\n")
		data = data[n:]
		cmd, capabilities, extra := strings.Cut(line, "\x00")
		if extra {
			if len(commands) != 0 {
				break
			}
			for capability := range strings.SplitSeq(capabilities, " ") {
				switch capability {
				case "report-status", "report-status-v2":
					opts.report = true
				case "side-band-64k":
					opts.sideband = true
				}
			}
		}
		if len(cmd) < 83 || cmd[40] != ' ' || cmd[81] != ' ' {
			break
		}
		old, oldOK := parseOID(cmd[:40])
		next, nextOK := parseOID(cmd[41:81])
		ref := cmd[82:]
		if !oldOK || !nextOK || !validRef(ref) || seen[ref] || (old == zeroID && next == zeroID) {
			break
		}
		commands = append(commands, refCommand{old, next, ref})
		seen[ref] = true
	}
	return nil, opts, nil, errors.New("invalid push commands")
}

func (o *gitObject) edges() ([]objectLink, error) {
	if !o.linksChecked {
		o.links, o.linksErr = readLinks(o.kind, o.data)
		o.linksChecked = true
	}
	return o.links, o.linksErr
}

func readLinks(kind objectKind, data []byte) ([]objectLink, error) {
	var links []objectLink
	if kind == blobKind {
		return nil, nil
	}
	if kind == treeKind {
		for len(data) != 0 {
			mode, rest, ok := bytes.Cut(data, []byte{' '})
			if !ok {
				return nil, errors.New("invalid tree mode")
			}
			name, rest, ok := bytes.Cut(rest, []byte{0})
			if !ok || len(rest) < len(oid{}) || len(name) == 0 || bytes.IndexByte(name, '/') >= 0 ||
				bytes.Equal(name, []byte(".")) || bytes.Equal(name, []byte("..")) {
				return nil, errors.New("invalid tree entry")
			}
			id := oid(rest[:len(oid{})])
			data = rest[len(id):]
			switch string(mode) {
			case "160000": // A submodule names a commit in another repository.
			case "40000", "040000":
				links = append(links, objectLink{id, treeKind})
			case "100644", "100755", "120000":
				links = append(links, objectLink{id, blobKind})
			default:
				return nil, errors.New("unsupported tree mode")
			}
		}
		return links, nil
	}
	header, _, ok := bytes.Cut(data, []byte("\n\n"))
	if !ok {
		return nil, errors.New("missing object header terminator")
	}
	first, rest, _ := bytes.Cut(header, []byte{'\n'})
	if kind == tagKind {
		line, _, _ := bytes.Cut(rest, []byte{'\n'})
		id, idOK := parseOID(strings.TrimPrefix(string(first), "object "))
		if !idOK || !bytes.HasPrefix(first, []byte("object ")) {
			return nil, errors.New("invalid tag object")
		}
		for i, kindName := range kindNames[1:] {
			if string(line) == "type "+kindName {
				return []objectLink{{id, objectKind(i + 1)}}, nil
			}
		}
		return nil, errors.New("invalid tag type")
	}
	tree, treeOK := parseOID(strings.TrimPrefix(string(first), "tree "))
	if !treeOK || !bytes.HasPrefix(first, []byte("tree ")) {
		return nil, errors.New("invalid commit tree")
	}
	links = append(links, objectLink{tree, treeKind})
	for len(rest) != 0 {
		line, more, _ := bytes.Cut(rest, []byte{'\n'})
		parent, ok := bytes.CutPrefix(line, []byte("parent "))
		if !ok {
			break
		}
		id, idOK := parseOID(string(parent))
		if !idOK {
			return nil, errors.New("invalid commit parent")
		}
		links = append(links, objectLink{id, commitKind})
		rest = more
	}
	return links, nil
}

func (g *gitRepo) graph(root objectLink, staged map[oid]*gitObject) ([]oid, error) {
	pending := []objectLink{root}
	seen := make(map[oid]bool)
	var reachable []oid
	for len(pending) != 0 {
		link := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		object, stored := g.objects[link.id]
		if !stored {
			object = staged[link.id]
		}
		if object == nil || (link.kind != 0 && object.kind != link.kind) {
			return nil, errors.New("missing or invalid object")
		}
		if stored || seen[link.id] {
			continue
		}
		seen[link.id] = true
		children, err := object.edges()
		if err != nil {
			return nil, err
		}
		reachable = append(reachable, link.id)
		pending = append(pending, children...)
	}
	return reachable, nil
}

func (g *gitRepo) isAncestor(old, tip oid, staged map[oid]*gitObject) bool {
	pending := []oid{tip}
	seen := make(map[oid]bool)
	for len(pending) > 0 {
		id := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if id == old {
			return true
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		object := g.objects[id]
		if object == nil {
			object = staged[id]
		}
		if object == nil || object.kind != commitKind {
			continue
		}
		parents, err := object.edges()
		if err != nil {
			return false
		}
		for _, link := range parents {
			if link.kind == commitKind {
				if link.id == old {
					return true
				}
				pending = append(pending, link.id)
			}
		}
	}
	return false
}

func (g *gitRepo) refConflicts(name string) bool {
	for i := strings.LastIndexByte(name, '/'); i >= len("refs/heads"); i = strings.LastIndexByte(name[:i], '/') {
		if _, exists := g.refs[name[:i]]; exists {
			return true
		}
	}
	prefix := name + "/"
	for existing := range g.refs {
		if strings.HasPrefix(existing, prefix) {
			return true
		}
	}
	return false
}

func (g *gitRepo) receive(body []byte) ([]byte, error) {
	commands, options, pack, err := parsePush(body)
	if err != nil {
		return nil, err
	}
	if len(commands) == 0 {
		if len(pack) != 0 {
			return nil, errors.New("unexpected bytes after flush")
		}
		return []byte("0000"), nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	var staged map[oid]*gitObject
	unpackNeeded := len(pack) != 0
	for _, cmd := range commands {
		unpackNeeded = unpackNeeded || cmd.next != zeroID
	}
	if unpackNeeded {
		staged, err = unpack(pack, g.objects)
	}
	report := appendPacket(nil, "unpack ok\n")
	if err != nil {
		report = appendPacket(report[:0], "unpack invalid pack\n")
	}
	for _, cmd := range commands {
		reason := "unpacker error"
		if err == nil {
			reason = g.updateRef(cmd, staged)
		}
		if reason == "" {
			report = appendPacket(report, "ok "+cmd.ref+"\n")
		} else {
			report = appendPacket(report, "ng "+cmd.ref+" "+reason+"\n")
		}
	}
	if !options.report {
		return []byte("0000"), nil
	}
	report = append(report, "0000"...)
	if options.sideband {
		return frameReport(report), nil
	}
	return report, nil
}

func (g *gitRepo) updateRef(cmd refCommand, staged map[oid]*gitObject) string {
	current := g.refs[cmd.ref]
	if current != cmd.old {
		return "stale info"
	}
	if cmd.next == zeroID {
		delete(g.refs, cmd.ref)
		g.advertisement = nil
		g.refsCache = uploadCache{}
		return ""
	}
	if cmd.next == current {
		return ""
	}
	if current == zeroID && g.refConflicts(cmd.ref) {
		return "ref name conflict"
	}
	root := objectLink{id: cmd.next}
	if strings.HasPrefix(cmd.ref, "refs/heads/") {
		root.kind = commitKind
	}
	reachable, err := g.graph(root, staged)
	if err != nil {
		return "missing or invalid object"
	}
	if current != zeroID && (root.kind != commitKind || !g.isAncestor(current, cmd.next, staged)) {
		return "non-fast-forward"
	}
	extra := 0
	for _, id := range reachable {
		extra += len(staged[id].data) + len(staged[id].compressed)
	}
	if extra > maxRepoBytes-g.bytes {
		return "object store is full"
	}
	if g.objects == nil {
		g.objects = make(map[oid]*gitObject)
	}
	for _, id := range reachable {
		g.objects[id] = staged[id]
	}
	g.bytes += extra
	if g.refs == nil {
		g.refs = make(map[string]oid)
	}
	g.refs[cmd.ref] = cmd.next
	g.advertisement = nil
	g.refsCache = uploadCache{}
	return ""
}

func (f *forge) gitAuth(r *http.Request) bool {
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(f.bearer)) == 1 {
		return true
	}
	_, password, ok := r.BasicAuth()
	return ok && subtle.ConstantTimeCompare([]byte(password), []byte(strings.TrimPrefix(f.bearer, "Bearer "))) == 1
}

func (f *forge) serveGit(w http.ResponseWriter, r *http.Request) {
	owner, path, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	name, path, found := strings.Cut(path, "/")
	repo, repoPath := strings.CutSuffix(name, ".git")
	advertise := path == "info/refs" && r.Method == http.MethodGet
	service := path
	if advertise {
		service = r.URL.Query().Get("service")
	}
	if !found || !repoPath || (service != "git-receive-pack" && service != "git-upload-pack") ||
		(!advertise && r.Method != http.MethodPost) {
		problem(w, http.StatusNotFound, "Not Found")
		return
	}

	if !f.gitAuth(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="Forge"`)
		problem(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	var body []byte
	if !advertise {
		limit := int64(maxPackBody)
		if service == "git-upload-pack" {
			limit = maxFetchRequest
		}
		var release func()
		var err error
		if service == "git-receive-pack" {
			body, release, err = readGitBody(w, r, limit)
			defer release()
		} else {
			body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
		}
		if err != nil {
			var cap *http.MaxBytesError
			if errors.As(err, &cap) {
				problem(w, http.StatusRequestEntityTooLarge, "Request body is too large")
			} else {
				problem(w, http.StatusBadRequest, "Malformed Git request")
			}
			return
		}
	}
	f.mu.RLock()
	target := f.repos[strings.ToLower(repo)]
	if target == nil || !f.owns(owner) {
		f.mu.RUnlock()
		problem(w, http.StatusNotFound, "Not Found")
		return
	}
	var out []byte
	var err error
	if service == "git-upload-pack" {
		v2 := slices.Contains(strings.Split(r.Header.Get("Git-Protocol"), ":"), "version=2")
		head := "refs/heads/" + target.Branch
		if advertise {
			out = target.git.advertiseUpload(head, v2)
		} else {
			out, err = target.git.upload(body, head, v2)
		}
	} else if advertise {
		out = target.git.advertise()
	} else {
		out, err = target.git.receive(body)
	}
	f.mu.RUnlock()
	if err != nil {
		problem(w, http.StatusBadRequest, "Malformed Git request")
		return
	}
	contentType := "application/x-" + service + "-result"
	if advertise {
		contentType = "application/x-" + service + "-advertisement"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	send(w, http.StatusOK, out)
}
