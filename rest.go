package main

import (
	"encoding/base64"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

func (f *forge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if e := recover(); e != nil {
			if fault, ok := e.(apiFault); ok {
				problem(w, fault.status, fault.message)
			} else {
				panic(e)
			}
		}
	}()
	path := r.URL.Path
	if path == "/healthz" && r.Method == "GET" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		send(w, 200, []byte(`{"ok":true}`))
		return
	}
	if path != "/api/graphql" && path != "/mcp" && path != "/api/v3" && !strings.HasPrefix(path, "/api/v3/") {
		f.serveGit(w, r)
		return
	}
	// The wire server can write the immutable Issue JSON without building a header map.
	if w, ok := w.(*wireResponse); ok && r.Method == "GET" && w.body == nil {
		if owner, name, number, matched := issueReadPath(path); matched && f.gitAuth(r) {
			if data := f.cachedIssue(owner, name, number); data != nil {
				w.writeIssue(data, f.ids.Add(1))
				return
			}
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if strings.HasPrefix(path, "/api/v3") {
		w.Header().Set("X-GitHub-Request-Id", strconv.FormatUint(f.ids.Add(1), 16))
	}
	if !f.gitAuth(r) {
		problem(w, 401, "Bad credentials")
		return
	}
	if path == "/mcp" && r.Method == "DELETE" {
		send(w, 204, nil)
		return
	}
	rpc := path == "/api/graphql" || path == "/mcp"
	if rpc && r.Method != "POST" {
		problem(w, 405, "Method Not Allowed")
		return
	}
	if r.Method == "POST" {
		if owner, name, resource, ok := issueRepoPath(path); ok && resource == "issues" {
			f.postIssue(w, r, owner, name)
			return
		}
	}
	var value any
	if r.Method != "GET" && r.Method != "HEAD" {
		if path == "/mcp" {
			if !decodeRPC(w, r, &value) {
				return
			}
		} else if !decode(w, r, &value) {
			return
		}
	}
	a := obj(value)
	// Read the immutable response under the store's read lock.
	if r.Method == "GET" {
		if owner, name, number, ok := issueReadPath(path); ok {
			data := f.cachedIssue(owner, name, number)
			if data == nil {
				absent()
			}
			send(w, 200, data)
			return
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// All other surfaces can consult and update Git under the Forge write lock.
	// The Git transport holds a Forge read lock before entering its repository lock.
	if path == "/api/graphql" {
		send(w, 200, encode(f.executeGraphQL(value)))
		return
	}
	if path == "/mcp" {
		status, reply := f.mcp(a)
		if reply == nil {
			send(w, status, nil)
		} else {
			send(w, status, encode(reply))
		}
		return
	}
	status, reply := f.rest(w, r, strings.TrimPrefix(path, "/api/v3"), a)
	if body, ok := reply.([]byte); ok {
		send(w, status, body)
	} else if status == 204 {
		send(w, status, nil)
	} else {
		send(w, status, encode(reply))
	}
}
func (f *forge) cachedIssue(owner, name string, number int) []byte {
	f.mu.RLock()
	defer f.mu.RUnlock()
	repo := f.repos[strings.ToLower(name)]
	if repo == nil || !f.owns(owner) {
		return nil
	}
	row := repo.issueRows[number]
	if row == nil {
		return nil
	}
	return row.cached
}

func issueRepoPath(path string) (string, string, string, bool) {
	rest, ok := strings.CutPrefix(path, "/api/v3/repos/")
	if !ok {
		return "", "", "", false
	}
	owner, rest, ok := strings.Cut(rest, "/")
	if !ok {
		return "", "", "", false
	}
	name, resource, ok := strings.Cut(rest, "/")
	return owner, name, resource, ok
}

func issueReadPath(path string) (string, string, int, bool) {
	owner, name, resource, ok := issueRepoPath(path)
	number, isIssue := strings.CutPrefix(resource, "issues/")
	if !ok || !isIssue {
		return "", "", 0, false
	}
	n := integerPath(number)
	return owner, name, n, n > 0
}

func (f *forge) postIssue(w http.ResponseWriter, r *http.Request, owner, name string) {
	raw, ok := requestBody(w, r)
	if !ok {
		return
	}
	title, body, simple := simpleIssueInput(raw)
	var input any
	if !simple && len(raw) != 0 && json.Unmarshal(raw, &input) != nil {
		problem(w, 400, "Malformed JSON")
		return
	}
	f.mu.Lock()
	reply := func() []byte {
		defer f.mu.Unlock()
		repo := f.repo(owner, name)
		if simple {
			return f.saveIssue(repo, reqText(title, "title"), body, []label{}).cached
		}
		return f.newIssue(repo, obj(input)).cached
	}()
	send(w, 201, reply)
}
func integerPath(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 || strconv.Itoa(n) != s {
		return 0
	}
	return n
}
func page[T any](w http.ResponseWriter, r *http.Request, rows []T) []T {
	q := r.URL.Query()
	n, _ := strconv.Atoi(q.Get("per_page"))
	if n == 0 {
		n = 30
	}
	n = min(100, max(1, n))
	p, _ := strconv.Atoi(q.Get("page"))
	p = max(1, p)
	if p > 1+len(rows)/n {
		return []T{}
	}
	first := min(len(rows), (p-1)*n)
	end := min(len(rows), first+n)
	if end < len(rows) {
		q.Set("page", strconv.Itoa(p+1))
		w.Header().Set("Link", "<http://"+r.Host+r.URL.EscapedPath()+"?"+q.Encode()+">; rel=\"next\"")
	}
	if end == first {
		return []T{}
	}
	return rows[first:end]
}
func refJSON(name string, id oid) args {
	return args{"ref": name, "object": args{"sha": id.String(), "type": "commit"}}
}
func (f *forge) rest(w http.ResponseWriter, r *http.Request, path string, a args) (int, any) {
	method := r.Method
	q := r.URL.Query()
	if (path == "" || path == "/") && method == "GET" {
		w.Header().Set("X-OAuth-Scopes", "repo, read:org")
		origin := "http://" + r.Host + "/api/v3"
		return 200, args{"current_user_url": origin + "/user", "repository_url": origin + "/repos/{owner}/{repo}"}
	}
	if path == "/meta" && method == "GET" {
		return 200, args{"installed_version": "3.0.0"}
	}
	if path == "/user" && method == "GET" {
		return 200, args{"login": f.ownerName(), "id": 1}
	}
	if path == "/user/repos" {
		if method == "POST" {
			return 201, f.repoJSON(f.newRepo(a))
		}
		if method == "GET" {
			rows := []args{}
			for _, repo := range f.repositories() {
				rows = append(rows, f.repoJSON(repo))
			}
			return 200, page(w, r, rows)
		}
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) < 3 || parts[0] != "repos" {
		absent()
	}
	repo := f.repo(parts[1], parts[2])
	g := &repo.git
	if len(parts) == 3 {
		switch method {
		case "GET":
			return 200, f.repoJSON(repo)
		case "DELETE":
			delete(f.repos, strings.ToLower(repo.Name))
			return 204, nil
		case "PATCH":
			if v, ok := a["description"]; ok {
				repo.Description = str(v)
			}
			if v, ok := a["private"].(bool); ok {
				repo.Private = v
			}
			return 200, f.repoJSON(repo)
		}
		absent()
	}
	tail := parts[4:]
	n := 0
	if len(tail) > 0 {
		n = integerPath(tail[0])
	}
	switch parts[3] {
	case "issues":
		if len(tail) == 0 {
			if method == "POST" {
				return 201, f.newIssue(repo, a).cached
			}
			if method == "GET" {
				return 200, page(w, r, issueList(repo, fallback(q.Get("state"), "open"), q.Get("direction") == "asc"))
			}
		}
		if n > 0 && len(tail) == 1 && method == "PATCH" {
			i := repo.lookupIssue(n)
			f.updateIssue(i, a)
			return 200, i.cached
		}
		if n > 0 && len(tail) == 2 && tail[1] == "comments" {
			repo.lookupIssue(n)
			if method == "POST" {
				return 201, f.addNote(repo, n, a)
			}
			if method == "GET" {
				return 200, page(w, r, repo.comments[n])
			}
		}
	case "labels":
		if len(tail) == 0 && method == "POST" {
			name := reqText(a["name"], "name")
			key := strings.ToLower(name)
			if _, exists := repo.labels[key]; exists {
				invalid("Label already exists")
			}
			l := label{name, fallback(str(a["color"]), "ededed"), str(a["description"])}
			repo.labels[key] = l
			return 201, l
		}
		if len(tail) == 0 && method == "GET" {
			out := []label{}
			for _, k := range slices.Sorted(maps.Keys(repo.labels)) {
				out = append(out, repo.labels[k])
			}
			return 200, page(w, r, out)
		}
	case "pulls":
		if len(tail) == 0 {
			if method == "POST" {
				return 201, f.newPull(repo, a)
			}
			if method == "GET" {
				return 200, page(w, r, pullList(repo, fallback(q.Get("state"), "open")))
			}
		}
		if n > 0 {
			p := repo.lookupPull(n)
			if len(tail) == 1 && method == "GET" {
				return 200, p
			}
			if len(tail) == 2 {
				if tail[1] == "files" && method == "GET" {
					return 200, page(w, r, g.diff(p))
				}
				if tail[1] == "merge" && method == "PUT" {
					return 200, f.mergePull(repo, p, a)
				}
			}
		}
	case "branches":
		if method == "GET" {
			branch := strings.Join(tail, "/")
			rows := []args{}
			for _, key := range g.refNames() {
				name, ok := strings.CutPrefix(key, "refs/heads/")
				if !ok {
					continue
				}
				row := args{"name": name, "commit": args{"sha": g.refs[key].String()}, "protected": false}
				if len(tail) > 0 && name == branch {
					return 200, row
				}
				rows = append(rows, row)
			}
			if len(tail) > 0 {
				absent()
			}
			return 200, page(w, r, rows)
		}
	case "commits":
		if method == "GET" && len(tail) <= 1 {
			name := fallback(q.Get("sha"), repo.Branch)
			if len(tail) > 0 {
				name = tail[0]
			}
			id := g.peel(g.resolve(name))
			if len(tail) > 0 {
				return 200, g.commitJSON(id)
			}
			if id == zeroID && q.Has("sha") {
				absent()
			}
			rows := []args{}
			seen := map[oid]bool{}
			pending := []oid{id}
			for i := 0; i < len(pending); i++ {
				key := pending[i]
				if key == zeroID || seen[key] {
					continue
				}
				seen[key] = true
				rows = append(rows, g.commitJSON(key))
				pending = append(pending, g.parents(key)...)
			}
			return 200, page(w, r, rows)
		}
	case "git":
		if len(tail) > 0 && (tail[0] == "refs" || tail[0] == "ref") {
			if len(tail) == 1 && method == "POST" {
				name := str(a["ref"])
				id, ok := parseOID(str(a["sha"]))
				o := g.objects[id]
				if !validRef(name) || !ok || o == nil || g.refs[name] != zeroID || g.refConflicts(name) || (strings.HasPrefix(name, "refs/heads/") && o.kind != commitKind) {
					invalid("Invalid or existing ref")
				}
				g.setRef(name, id)
				return 201, refJSON(name, id)
			}
			if len(tail) > 1 && method == "GET" {
				name := "refs/" + strings.TrimPrefix(strings.Join(tail[1:], "/"), "refs/")
				id := g.refs[name]
				if id == zeroID {
					absent()
				}
				return 200, refJSON(name, id)
			}
		}
	case "contents":
		path := strings.Join(tail, "/")
		if method == "GET" {
			return 200, g.contents(path, g.resolve(fallback(q.Get("ref"), repo.Branch)))
		}
		if method == "PUT" || method == "DELETE" {
			return f.updateFile(repo, path, a, method == "DELETE")
		}
	}
	absent()
	return 0, nil
}
func (g *gitRepo) commitJSON(id oid) args {
	o := g.objects[id]
	if o == nil || o.kind != commitKind {
		absent()
	}
	links, _ := o.edges()
	if len(links) == 0 {
		absent()
	}
	parents := []args{}
	for _, e := range links[1:] {
		parents = append(parents, args{"sha": e.id.String()})
	}
	_, msg, _ := strings.Cut(string(o.data), "\n\n")
	return args{"sha": id.String(), "commit": args{"tree": args{"sha": links[0].id.String()}, "message": strings.TrimSuffix(msg, "\n")}, "parents": parents}
}
func (g *gitRepo) contents(path string, id oid) any {
	o := g.objects[g.peel(id)]
	if o == nil || o.kind != commitKind {
		absent()
	}
	links, _ := o.edges()
	if len(links) == 0 {
		absent()
	}
	id = links[0].id
	if path != "" {
		parts := strings.Split(path, "/")
		for i, name := range parts {
			o = g.objects[id]
			if o == nil || o.kind != treeKind {
				absent()
			}
			found := false
			for _, e := range treeEntries(o.data) {
				if e.name != name {
					continue
				}
				found = true
				id = e.id
				if i == len(parts)-1 && e.mode != "40000" && e.mode != "040000" {
					blob := g.objects[id]
					if blob == nil {
						absent()
					}
					return args{"type": "file", "name": name, "path": path, "sha": id.String(), "size": len(blob.data), "encoding": "base64", "content": base64.StdEncoding.EncodeToString(blob.data)}
				}
				break
			}
			if !found {
				absent()
			}
		}
	}
	o = g.objects[id]
	if o == nil || o.kind != treeKind {
		absent()
	}
	result := []args{}
	for _, e := range treeEntries(o.data) {
		kind := "file"
		if e.mode == "40000" || e.mode == "040000" {
			kind = "dir"
		}
		name := e.name
		if path != "" {
			name = path + "/" + name
		}
		result = append(result, args{"type": kind, "name": e.name, "path": name, "sha": e.id.String()})
	}
	return result
}
func (f *forge) updateFile(r *repository, path string, a args, remove bool) (int, any) {
	if path == "" || len(path) > 4096 || strings.ContainsRune(path, 0) {
		invalid("Invalid file path")
	}
	for _, c := range strings.Split(path, "/") {
		if c == "" || c == "." || c == ".." {
			invalid("Invalid file path")
		}
	}
	branch := fallback(str(a["branch"]), r.Branch)
	if !validRef(ref(branch)) {
		invalid("Invalid branch")
	}
	g := &r.git
	p := g.refs[ref(branch)]
	if p == zeroID && branch != r.Branch {
		absent()
	}
	files := g.filesAt(p)
	old, existed := files[path]
	sha, provided := a["sha"]
	if remove && !existed {
		absent()
	}
	if (existed && str(sha) != old.id.String()) || (!existed && provided) {
		fail(409, "File SHA does not match")
	}
	message := reqText(a["message"], "message")
	if remove {
		delete(files, path)
	} else {
		content, ok := a["content"].(string)
		if !ok {
			invalid("Invalid base64 content")
		}
		data, err := base64Data(content)
		if err != nil {
			invalid("Invalid base64 content")
		}
		for n := range files {
			if strings.HasPrefix(n, path+"/") || strings.HasPrefix(path, n+"/") {
				invalid("File and directory share a path")
			}
		}
		files[path] = file{fallback(old.mode, "100644"), g.put(blobKind, data)}
	}
	id := g.writeCommit(files, branch, p, message, f.timestamp())
	result := args{"commit": args{"sha": id.String()}, "content": nil}
	if !remove {
		result["content"] = g.contents(path, id)
	}
	if existed {
		return 200, result
	}
	return 201, result
}
