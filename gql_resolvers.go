package main

import (
	"slices"
	"strings"
)

func userGraph(name string) *graphNode {
	return node("User", args{"id": nodeID("User", 1), "login": name})
}
func noteGraph(c note) *graphNode {
	return node("IssueComment", args{"id": c.NodeID, "body": c.Body, "createdAt": c.At, "author": userGraph(c.User.Login)})
}
func issueGraph(r *repository, i *issue) *graphNode {
	if i == nil {
		return nil
	}
	return node("Issue", args{"id": i.NodeID, "databaseId": i.ID, "number": i.Number, "title": i.Title, "body": i.Body, "state": strings.ToUpper(i.State), "author": userGraph(i.User.Login), "createdAt": i.CreatedAt, "updatedAt": i.UpdatedAt, "closedAt": i.ClosedAt,
		"comments": resolve(func(a args) any {
			items := []*graphNode{}
			for _, c := range r.comments[i.Number] {
				items = append(items, noteGraph(c))
			}
			return connection("IssueComment", items, a)
		}), "labels": resolve(func(a args) any {
			items := []*graphNode{}
			for _, l := range i.Labels {
				items = append(items, node("Label", args{"name": l.Name, "color": l.Color, "description": l.Description}))
			}
			return connection("Label", items, a)
		})})
}
func pullState(p *pull) string {
	if p.Merged {
		return "MERGED"
	}
	return strings.ToUpper(p.State)
}
func pullGraph(r *repository, p *pull) *graphNode {
	if p == nil {
		return nil
	}
	return node("PullRequest", args{"id": p.NodeID, "databaseId": p.ID, "number": p.Number, "title": p.Title, "body": p.Body,
		"headRefName": p.Head.Ref, "headRefOid": p.Head.SHA, "baseRefName": p.Base.Ref, "baseRefOid": p.Base.SHA,
		"mergeable": resolve(func(args) any {
			if r.git.mergedFiles(p) == nil {
				return "CONFLICTING"
			}
			return "MERGEABLE"
		}), "state": pullState(p), "merged": p.Merged, "mergedAt": p.MergedAt,
		"files": resolve(func(a args) any {
			items := []*graphNode{}
			for _, d := range r.git.diff(p) {
				items = append(items, node("PullRequestChangedFile", args{"path": d.Filename, "additions": d.Additions, "deletions": d.Deletions, "changeType": strings.ToUpper(d.Status)}))
			}
			return connection("PullRequestChangedFile", items, a)
		})})
}
func hasState(a args, state string) bool {
	states, _ := a["states"].([]any)
	return len(states) == 0 || slices.ContainsFunc(states, func(v any) bool { return str(v) == state })
}
func (f *forge) repoGraph(r *repository) *graphNode {
	if r == nil {
		return nil
	}
	var branch *graphNode
	if r.git.refs[ref(r.Branch)] != zeroID {
		branch = node("Ref", args{"name": r.Branch})
	}
	return node("Repository", args{"id": r.NodeID, "databaseId": r.ID, "name": r.Name, "nameWithOwner": f.ownerName() + "/" + r.Name,
		"owner": userGraph(f.ownerName()), "description": r.Description, "isPrivate": r.Private, "viewerPermission": "ADMIN", "hasIssuesEnabled": true, "defaultBranchRef": branch,
		"issue": resolve(func(a args) any { return issueGraph(r, r.issueRows[num(a["number"])]) }),
		"issues": resolve(func(a args) any {
			asc := obj(a["orderBy"])["direction"] != "DESC"
			items := []*graphNode{}
			for _, i := range issueList(r, "all", asc) {
				if hasState(a, strings.ToUpper(i.State)) {
					items = append(items, issueGraph(r, i))
				}
			}
			return connection("Issue", items, a)
		}),
		"pullRequest": resolve(func(a args) any { return pullGraph(r, r.pulls[num(a["number"])]) }),
		"pullRequests": resolve(func(a args) any {
			items := []*graphNode{}
			rows := pullList(r, "all")
			slices.Reverse(rows)
			for _, p := range rows {
				if hasState(a, pullState(p)) {
					items = append(items, pullGraph(r, p))
				}
			}
			return connection("PullRequest", items, a)
		})})
}

var schema = map[string]string{
	"Repository":  "id databaseId name nameWithOwner owner description isPrivate defaultBranchRef viewerPermission hasIssuesEnabled issue issues pullRequests pullRequest",
	"Issue":       "id databaseId number title body state author createdAt updatedAt closedAt comments labels",
	"PullRequest": "id databaseId number title body headRefName baseRefName headRefOid baseRefOid mergeable state merged mergedAt files",
	"User":        "id login", "Query": "viewer repository node search", "Mutation": "createIssue closeIssue createPullRequest mergePullRequest addComment",
}

func introspect(name string) *graphNode {
	kind := "OBJECT"
	fields, title, ok := schema[name], "__Field", false
	if name == "SearchType" {
		kind = "ENUM"
		title = "__EnumValue"
		fields = "ISSUE REPOSITORY USER DISCUSSION"
		ok = true
	} else {
		_, ok = schema[name]
	}
	if !ok {
		return nil
	}
	out := []*graphNode{}
	for _, f := range strings.Fields(fields) {
		out = append(out, node(title, args{"name": f, "description": nil, "isDeprecated": false}))
	}
	a := args{"name": name, "kind": kind, "description": nil, "fields": nil, "enumValues": nil}
	if kind == "ENUM" {
		a["enumValues"] = out
	} else {
		a["fields"] = out
	}
	return node("__Type", a)
}
func (f *forge) lookupNode(id string) (*repository, *issue, *pull) {
	if id == "" {
		return nil, nil, nil
	}
	for _, r := range f.repos {
		if r.NodeID == id {
			return r, nil, nil
		}
		for _, i := range r.issueRows {
			if i.NodeID == id {
				return r, i, nil
			}
		}
		for _, p := range r.pulls {
			if p.NodeID == id {
				return r, nil, p
			}
		}
	}
	return nil, nil, nil
}
func (f *forge) queryGraph() *graphNode {
	return node("Query", args{"viewer": userGraph(f.ownerName()),
		"repository": resolve(func(a args) any {
			if !strings.EqualFold(str(a["owner"]), f.ownerName()) {
				return nil
			}
			return f.repoGraph(f.repos[strings.ToLower(str(a["name"]))])
		}),
		"node": resolve(func(a args) any {
			r, i, p := f.lookupNode(str(a["id"]))
			if i != nil {
				return issueGraph(r, i)
			}
			if p != nil {
				return pullGraph(r, p)
			}
			return f.repoGraph(r)
		}),
		"__type": resolve(func(a args) any { return introspect(str(a["name"])) }),
		"search": resolve(func(a args) any {
			term := strings.ToLower(str(a["query"]))
			kind := str(a["type"])
			items := []*graphNode{}
			for _, r := range f.repositories() {
				switch kind {
				case "REPOSITORY":
					if strings.Contains(strings.ToLower(r.Name), term) {
						items = append(items, f.repoGraph(r))
					}
				case "ISSUE":
					for _, i := range issueList(r, "all", true) {
						if strings.Contains(strings.ToLower(i.Title+" "+i.Body), term) {
							items = append(items, issueGraph(r, i))
						}
					}
				}
			}
			c := connection("SearchResultItem", items, a)
			c.fields["issueCount"] = 0
			c.fields["repositoryCount"] = 0
			if kind == "ISSUE" {
				c.fields["issueCount"] = len(items)
			}
			if kind == "REPOSITORY" {
				c.fields["repositoryCount"] = len(items)
			}
			return c
		})})
}
func (f *forge) mutationGraph() *graphNode {
	return node("Mutation", args{
		"createIssue": resolve(func(a args) any {
			in := obj(a["input"])
			r, i, p := f.lookupNode(str(in["repositoryId"]))
			if r == nil || i != nil || p != nil {
				absent()
			}
			return node("CreateIssuePayload", args{"issue": issueGraph(r, f.newIssue(r, in)), "clientMutationId": in["clientMutationId"]})
		}),
		"closeIssue": resolve(func(a args) any {
			in := obj(a["input"])
			r, i, _ := f.lookupNode(str(in["issueId"]))
			if i == nil {
				absent()
			}
			f.updateIssue(i, args{"state": "closed"})
			return node("CloseIssuePayload", args{"issue": issueGraph(r, i), "clientMutationId": in["clientMutationId"]})
		}),
		"addComment": resolve(func(a args) any {
			in := obj(a["input"])
			r, i, _ := f.lookupNode(str(in["subjectId"]))
			if i == nil {
				absent()
			}
			edge := node("IssueCommentEdge", args{"node": noteGraph(f.addNote(r, i.Number, in))})
			return node("AddCommentPayload", args{"commentEdge": edge, "clientMutationId": in["clientMutationId"]})
		}),
		"createPullRequest": resolve(func(a args) any {
			in := obj(a["input"])
			r, i, p := f.lookupNode(str(in["repositoryId"]))
			if r == nil || i != nil || p != nil {
				absent()
			}
			in["head"] = in["headRefName"]
			in["base"] = in["baseRefName"]
			return node("CreatePullRequestPayload", args{"pullRequest": pullGraph(r, f.newPull(r, in)), "clientMutationId": in["clientMutationId"]})
		}),
		"mergePullRequest": resolve(func(a args) any {
			in := obj(a["input"])
			r, _, p := f.lookupNode(str(in["pullRequestId"]))
			if p == nil {
				absent()
			}
			f.mergePull(r, p, args{"sha": in["expectedHeadOid"], "commit_title": in["commitHeadline"]})
			return node("MergePullRequestPayload", args{"pullRequest": pullGraph(r, p), "clientMutationId": in["clientMutationId"]})
		}),
	})
}
