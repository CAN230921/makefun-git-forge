package main

import (
	"encoding/base64"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

type args map[string]any
type apiFault struct {
	status  int
	message string
}

func fail(status int, message string) { panic(apiFault{status, message}) }
func absent()                         { fail(404, "Not Found") }
func invalid(message string)          { fail(422, message) }
func str(v any) string                { s, _ := v.(string); return s }
func reqText(v any, name string) string {
	s := str(v)
	if strings.TrimSpace(s) == "" {
		invalid(name + " is required")
	}
	return s
}
func obj(v any) args {
	if a, ok := v.(map[string]any); ok {
		return a
	}
	if a, ok := v.(args); ok {
		return a
	}
	return nil
}
func num(v any) int {
	switch n := v.(type) {
	case float64:
		if n == float64(int(n)) {
			return int(n)
		}
	case int:
		return n
	}
	return 0
}
func fallback(s, d string) string {
	if s != "" {
		return s
	}
	return d
}
func (f *forge) allocate() uint64 { f.nextID++; return f.nextID }
func (f *forge) timestamp() int64 { t := f.second; f.second++; return t }
func iso(s int64) string          { return time.Unix(s, 0).UTC().Format("2006-01-02T15:04:05.000Z") }
func (f *forge) date() string     { return iso(f.timestamp()) }

type label struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}
type note struct {
	ID     uint64 `json:"id"`
	NodeID string `json:"node_id"`
	Body   string `json:"body"`
	At     string `json:"created_at"`
	User   login  `json:"user"`
}
type refView struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}
type pull struct {
	ID       uint64  `json:"id"`
	NodeID   string  `json:"node_id"`
	Number   int     `json:"number"`
	Title    string  `json:"title"`
	Body     string  `json:"body"`
	State    string  `json:"state"`
	Merged   bool    `json:"merged"`
	MergedAt *string `json:"merged_at"`
	MergeSHA *string `json:"merge_commit_sha"`
	Head     refView `json:"head"`
	Base     refView `json:"base"`
	At       string  `json:"created_at"`
}
type file struct {
	mode string
	id   oid
}
type treeEntry struct {
	name string
	file
}
type fileDiff struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Changes   int    `json:"changes"`
}

func ref(branch string) string { return "refs/heads/" + branch }
func (f *forge) repo(owner, name string) *repository {
	r := f.repos[strings.ToLower(name)]
	if r == nil || !f.owns(owner) {
		absent()
	}
	return r
}
func (f *forge) repoJSON(r *repository) args {
	owner := f.ownerName()
	return args{"id": r.ID, "node_id": r.NodeID, "name": r.Name, "full_name": owner + "/" + r.Name, "owner": login{owner}, "private": r.Private, "description": r.Description, "default_branch": r.Branch, "has_issues": r.HasIssues}
}
func (f *forge) repositories() []*repository {
	rows := slices.Collect(maps.Values(f.repos))
	slices.SortFunc(rows, func(a, b *repository) int { return int(a.ID) - int(b.ID) })
	return rows
}
func (f *forge) newRepo(a args) *repository {
	name := reqText(a["name"], "name")
	key := strings.ToLower(name)
	branch := fallback(str(a["default_branch"]), "main")
	if f.repos[key] != nil {
		invalid("Repository already exists")
	}
	if !repoName.MatchString(name) || name == "." || name == ".." || !validRef(ref(branch)) {
		invalid("Invalid or existing repository")
	}
	id := f.allocate()
	private, _ := a["private"].(bool)
	r := &repository{ID: id, NodeID: nodeID("Repository", id), Name: name, FullName: f.ownerName() + "/" + name, Owner: login{f.ownerName()}, Private: private, Description: str(a["description"]), Branch: branch, HasIssues: true, nextNumber: 1,
		issueRows: make(map[int]*issue), pulls: make(map[int]*pull), labels: make(map[string]label), comments: make(map[int][]note)}
	if a["auto_init"] == true {
		id := r.git.put(blobKind, []byte("# "+name+"\n"))
		r.git.writeCommit(map[string]file{"README.md": {"100644", id}}, branch, zeroID, "Initial commit", f.timestamp())
	}
	f.repos[key] = r
	return r
}
func (r *repository) lookupIssue(n int) *issue {
	i := r.issueRows[n]
	if i == nil {
		absent()
	}
	return i
}
func (r *repository) lookupPull(n int) *pull {
	p := r.pulls[n]
	if p == nil {
		absent()
	}
	return p
}
func (f *forge) newIssue(r *repository, a args) *issue {
	title := reqText(a["title"], "title")
	labels := []label{}
	if names, ok := a["labels"].([]any); ok {
		for _, name := range names {
			l, ok := r.labels[strings.ToLower(str(name))]
			if !ok {
				invalid("Unknown issue label")
			}
			labels = append(labels, l)
		}
	}
	return f.saveIssue(r, title, str(a["body"]), labels)
}

func (f *forge) saveIssue(r *repository, title, body string, labels []label) *issue {
	id := f.allocate()
	at := f.date()
	n := r.nextNumber
	r.nextNumber++
	v := &issue{ID: id, NodeID: nodeID("Issue", id), Number: n, Title: title, Body: body, State: "open", Labels: labels, User: login{f.ownerName()}, CreatedAt: at, UpdatedAt: at}
	r.issueRows[n] = v
	v.cached = encode(v)
	return v
}
func (f *forge) updateIssue(i *issue, a args) {
	s, found := a["state"]
	if found && s != "open" && s != "closed" {
		invalid("Invalid issue state")
	}
	if title, ok := a["title"]; ok {
		i.Title = reqText(title, "title")
	}
	if body, ok := a["body"]; ok {
		i.Body = str(body)
	}
	at := f.date()
	if found && str(s) != i.State {
		i.State = str(s)
		i.ClosedAt = nil
		if i.State == "closed" {
			i.ClosedAt = &at
		}
	}
	i.UpdatedAt = at
	i.cached = encode(i)
}
func (f *forge) addNote(r *repository, n int, a args) note {
	i := r.lookupIssue(n)
	body := reqText(a["body"], "body")
	id := f.allocate()
	c := note{id, nodeID("IssueComment", id), body, f.date(), login{f.ownerName()}}
	r.comments[n] = append(r.comments[n], c)
	i.Comments = len(r.comments[n])
	i.UpdatedAt = c.At
	i.cached = encode(i)
	return c
}
func issueList(r *repository, state string, asc bool) []*issue {
	out := []*issue{}
	for _, v := range r.issueRows {
		if state == "all" || v.State == state {
			out = append(out, v)
		}
	}
	slices.SortFunc(out, func(a, b *issue) int {
		if asc {
			return a.Number - b.Number
		}
		return b.Number - a.Number
	})
	return out
}
func pullList(r *repository, state string) []*pull {
	out := []*pull{}
	for _, v := range r.pulls {
		if state == "all" || v.State == state {
			out = append(out, v)
		}
	}
	slices.SortFunc(out, func(a, b *pull) int { return b.Number - a.Number })
	return out
}
func (g *gitRepo) put(kind objectKind, data []byte) oid {
	o := &gitObject{kind: kind, data: data}
	id := o.id()
	if g.objects == nil {
		g.objects = make(map[oid]*gitObject)
	}
	if g.objects[id] == nil {
		if len(data) > maxRepoBytes-g.bytes {
			invalid("Object store is full")
		}
		g.objects[id] = o
		g.bytes += len(data)
	}
	return id
}
func (g *gitRepo) setRef(name string, id oid) {
	if g.refs == nil {
		g.refs = make(map[string]oid)
	}
	g.refs[name] = id
	g.advertisement = nil
	g.refsCache.Store(nil)
}
func (g *gitRepo) resolve(name string) oid {
	for _, key := range []string{name, ref(name), "refs/tags/" + name} {
		if v := g.refs[key]; v != zeroID {
			return v
		}
	}
	id, _ := parseOID(name)
	if g.objects[id] != nil {
		return id
	}
	return zeroID
}
func treeEntries(data []byte) []treeEntry {
	out := []treeEntry{}
	for len(data) > 0 {
		mode, rest, ok := strings.Cut(string(data), " ")
		if !ok {
			break
		}
		name, rest, ok := strings.Cut(rest, "\x00")
		if !ok || len(rest) < 20 {
			break
		}
		out = append(out, treeEntry{name, file{mode, oid([]byte(rest[:20]))}})
		data = []byte(rest[20:])
	}
	return out
}
func (g *gitRepo) filesAt(id oid) map[string]file {
	out := make(map[string]file)
	c := g.objects[g.peel(id)]
	if c == nil || c.kind != commitKind {
		return out
	}
	links, _ := c.edges()
	if len(links) == 0 {
		return out
	}
	type step struct {
		prefix string
		id     oid
	}
	queue := []step{{"", links[0].id}}
	for len(queue) > 0 {
		k := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		o := g.objects[k.id]
		if o == nil {
			continue
		}
		for _, e := range treeEntries(o.data) {
			path := k.prefix + e.name
			if e.mode == "40000" || e.mode == "040000" {
				queue = append(queue, step{path + "/", e.id})
			} else {
				out[path] = e.file
			}
		}
	}
	return out
}
func (g *gitRepo) writeTree(files map[string]file) oid {
	type dir struct {
		files map[string]file
		dirs  map[string]*dir
	}
	newDir := func() *dir { return &dir{make(map[string]file), make(map[string]*dir)} }
	root := newDir()
	for path, v := range files {
		p := strings.Split(path, "/")
		d := root
		for _, name := range p[:len(p)-1] {
			if _, ok := d.files[name]; ok {
				invalid("File and directory share a path")
			}
			if d.dirs[name] == nil {
				d.dirs[name] = newDir()
			}
			d = d.dirs[name]
		}
		name := p[len(p)-1]
		if d.dirs[name] != nil {
			invalid("File and directory share a path")
		}
		d.files[name] = v
	}
	var build func(*dir) oid
	build = func(d *dir) oid {
		names := slices.Collect(maps.Keys(d.files))
		for n := range d.dirs {
			names = append(names, n+"/")
		}
		slices.Sort(names)
		var data []byte
		for _, k := range names {
			name := k
			v := d.files[k]
			if n, ok := strings.CutSuffix(k, "/"); ok {
				name = n
				v = file{"40000", build(d.dirs[n])}
			}
			data = append(data, v.mode+" "+name+"\x00"...)
			data = append(data, v.id[:]...)
		}
		return g.put(treeKind, data)
	}
	return build(root)
}
func (g *gitRepo) writeCommit(files map[string]file, branch string, parent oid, message string, t int64) oid {
	out := "tree " + g.writeTree(files).String() + "\n"
	if parent != zeroID {
		out += "parent " + parent.String() + "\n"
	}
	ident := fmt.Sprintf("Forge <forge@localhost> %d +0000", t)
	id := g.put(commitKind, []byte(out+"author "+ident+"\ncommitter "+ident+"\n\n"+message+"\n"))
	g.setRef(ref(branch), id)
	return id
}
func (g *gitRepo) parents(id oid) []oid {
	c := g.objects[id]
	if c == nil || c.kind != commitKind {
		return nil
	}
	links, _ := c.edges()
	var out []oid
	for _, l := range links {
		if l.kind == commitKind {
			out = append(out, l.id)
		}
	}
	return out
}
func (g *gitRepo) mergeBase(a, b oid) oid {
	seen := map[oid]bool{}
	queue := []oid{a}
	for i := 0; i < len(queue); i++ {
		id := queue[i]
		if !seen[id] {
			seen[id] = true
			queue = append(queue, g.parents(id)...)
		}
	}
	done := map[oid]bool{}
	queue = []oid{b}
	for i := 0; i < len(queue); i++ {
		id := queue[i]
		if seen[id] {
			return id
		}
		if !done[id] {
			done[id] = true
			queue = append(queue, g.parents(id)...)
		}
	}
	return zeroID
}
func (f *forge) newPull(r *repository, a args) *pull {
	title := reqText(a["title"], "title")
	head := reqText(a["head"], "head")
	head = head[strings.LastIndexByte(head, ':')+1:]
	base := reqText(a["base"], "base")
	h, b := r.git.refs[ref(head)], r.git.refs[ref(base)]
	if h == zeroID || b == zeroID || head == base {
		invalid("Invalid head or base branch")
	}
	id := f.allocate()
	n := r.nextNumber
	r.nextNumber++
	p := &pull{ID: id, NodeID: nodeID("PullRequest", id), Number: n, Title: title, Body: str(a["body"]), State: "open", Head: refView{head, h.String()}, Base: refView{base, b.String()}, At: f.date()}
	r.pulls[n] = p
	return p
}
func (g *gitRepo) pullTips(p *pull) (oid, oid) {
	b, _ := parseOID(p.Base.SHA)
	h, _ := parseOID(p.Head.SHA)
	if !p.Merged {
		if id := g.refs[ref(p.Base.Ref)]; id != zeroID {
			b = id
		}
		if id := g.refs[ref(p.Head.Ref)]; id != zeroID {
			h = id
		}
	}
	return b, h
}
func (g *gitRepo) mergedFiles(p *pull) map[string]file {
	b, h := g.pullTips(p)
	old := g.filesAt(g.mergeBase(b, h))
	to := g.filesAt(b)
	from := g.filesAt(h)
	names := make(map[string]bool)
	for n := range old {
		names[n] = true
	}
	for n := range from {
		names[n] = true
	}
	for n := range names {
		if old[n] == from[n] {
			continue
		}
		if to[n] != old[n] && to[n] != from[n] {
			return nil
		}
		if v, ok := from[n]; ok {
			to[n] = v
		} else {
			delete(to, n)
		}
	}
	for n := range to {
		for i := strings.LastIndexByte(n, '/'); i >= 0; i = strings.LastIndexByte(n[:i], '/') {
			if _, ok := to[n[:i]]; ok {
				return nil
			}
		}
	}
	return to
}
func (f *forge) mergePull(r *repository, p *pull, a args) args {
	g := &r.git
	if p.State != "open" || g.refs[ref(p.Head.Ref)] == zeroID || g.refs[ref(p.Base.Ref)] == zeroID {
		fail(405, "Pull request cannot be merged")
	}
	files := g.mergedFiles(p)
	if files == nil {
		fail(405, "Pull request has conflicts")
	}
	b, h := g.pullTips(p)
	if expected := str(a["sha"]); expected != "" && expected != h.String() {
		fail(409, "Head SHA changed")
	}
	t := f.timestamp()
	id := g.writeCommit(files, p.Base.Ref, b, fallback(str(a["commit_title"]), fmt.Sprintf("%s (#%d)", p.Title, p.Number)), t).String()
	at := iso(t)
	p.Base.SHA = b.String()
	p.Head.SHA = h.String()
	p.State = "closed"
	p.Merged = true
	p.MergedAt = &at
	p.MergeSHA = &id
	return args{"sha": id, "merged": true, "message": "Pull Request successfully merged"}
}
func (g *gitRepo) diff(p *pull) []fileDiff {
	b, h := g.pullTips(p)
	old, new := g.filesAt(g.mergeBase(b, h)), g.filesAt(h)
	names := map[string]bool{}
	for n := range old {
		names[n] = true
	}
	for n := range new {
		names[n] = true
	}
	out := []fileDiff{}
	for _, n := range slices.Sorted(maps.Keys(names)) {
		a, hasA := old[n]
		z, hasZ := new[n]
		if a == z {
			continue
		}
		kind := "modified"
		if !hasA {
			kind = "added"
		}
		if !hasZ {
			kind = "removed"
		}
		var left, right []byte
		if o := g.objects[a.id]; o != nil {
			left = o.data
		}
		if o := g.objects[z.id]; o != nil {
			right = o.data
		}
		adds, dels := lineDiff(left, right)
		out = append(out, fileDiff{n, kind, adds, dels, adds + dels})
	}
	return out
}
func lineDiff(a, b []byte) (int, int) {
	lines := func(s []byte) []string {
		if len(s) == 0 {
			return nil
		}
		return strings.Split(strings.TrimSuffix(string(s), "\n"), "\n")
	}
	left, right := lines(a), lines(b)
	if len(left) > 0 && len(right) > 1000000/len(left) {
		return len(right), len(left)
	}
	dp := make([]int, len(right)+1)
	for _, x := range left {
		prev := 0
		for j, y := range right {
			saved := dp[j+1]
			if x == y {
				dp[j+1] = prev + 1
			} else {
				dp[j+1] = max(dp[j], dp[j+1])
			}
			prev = saved
		}
	}
	return len(right) - dp[len(right)], len(left) - dp[len(right)]
}
func base64Data(data string) ([]byte, error) {
	data = strings.Map(func(r rune) rune {
		if strings.ContainsRune(" \t\r\n", r) {
			return -1
		}
		return r
	}, data)
	if strings.Contains(data, "=") {
		return base64.StdEncoding.DecodeString(data)
	}
	return base64.RawStdEncoding.DecodeString(data)
}
