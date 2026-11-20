package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
)

type tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Schema      args   `json:"inputSchema"`
}

func defineTool(name, description string, required []string, properties string) tool {
	fields := args{}
	for _, item := range strings.Fields(properties) {
		k, t, ok := strings.Cut(item, ":")
		if !ok {
			t = "string"
		}
		fields[k] = args{"type": t}
	}
	return tool{name, description, args{"type": "object", "properties": fields, "required": required}}
}

var tools = []tool{
	defineTool("create_repo", "Create a Git repository", []string{"name"}, "name description private:boolean auto_init:boolean"),
	defineTool("get_repo", "Read a repository", []string{"owner", "repo"}, "owner repo"),
	defineTool("create_issue", "Open a repository issue", []string{"owner", "repo", "title"}, "owner repo title body"),
	defineTool("list_issues", "List repository issues by state", []string{"owner", "repo"}, "owner repo state"),
	defineTool("create_pull_request", "Open a pull request", []string{"owner", "repo", "title", "head", "base"}, "owner repo title head base body"),
	defineTool("merge_pull_request", "Squash a pull request", []string{"owner", "repo", "pull_number"}, "owner repo pull_number:integer sha commit_title"),
}
var rpcVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26"}

func rpcError(id any, code int, message string) args {
	return args{"jsonrpc": "2.0", "id": id, "error": args{"code": code, "message": message}}
}
func decodeRPC(w http.ResponseWriter, r *http.Request, v *any) bool {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	var size *http.MaxBytesError
	if errors.As(err, &size) {
		problem(w, 413, "Request body is too large")
		return false
	}
	if err != nil || json.Unmarshal(raw, v) != nil {
		send(w, 400, encode(rpcError(nil, -32700, "Parse error")))
		return false
	}
	return true
}
func (f *forge) mcp(body args) (int, any) {
	method := str(body["method"])
	id := body["id"]
	switch id.(type) {
	case string, float64:
	default:
		id = nil
	}
	if body["jsonrpc"] != "2.0" || method == "" {
		return 400, rpcError(id, -32600, "Invalid Request")
	}
	if id == nil {
		return 202, nil
	}
	result := func(v any) args { return args{"jsonrpc": "2.0", "id": id, "result": v} }
	params := obj(body["params"])
	switch method {
	case "server/discover", "initialize":
		info := args{"serverInfo": args{"name": "forge", "version": "1.0.0"}, "capabilities": args{"tools": args{"listChanged": false}}}
		if method == "server/discover" {
			info["supportedVersions"] = rpcVersions
		} else {
			v := str(params["protocolVersion"])
			if !slices.Contains(rpcVersions, v) {
				v = rpcVersions[1]
			}
			info["protocolVersion"] = v
		}
		return 200, result(info)
	case "ping":
		return 200, result(args{})
	case "tools/list":
		return 200, result(args{"tools": tools})
	case "tools/call":
		name := str(params["name"])
		i := slices.IndexFunc(tools, func(t tool) bool { return t.Name == name })
		if i < 0 {
			return 200, rpcError(id, -32602, "Unknown tool")
		}
		a := obj(params["arguments"])
		for _, key := range tools[i].Schema["required"].([]string) {
			if (key == "pull_number" && num(a[key]) <= 0) || (key != "pull_number" && strings.TrimSpace(str(a[key])) == "") {
				return 200, rpcError(id, -32602, "Missing or invalid argument: "+key)
			}
		}
		return 200, result(f.callTool(name, a))
	default:
		return 200, rpcError(id, -32601, "Method not found")
	}
}
func (f *forge) callTool(name string, a args) (reply args) {
	defer func() {
		if e := recover(); e != nil {
			if err, ok := e.(apiFault); ok {
				reply = args{"isError": true, "content": []args{{"type": "text", "text": err.message}}}
			} else {
				panic(e)
			}
		}
	}()
	var data any
	if name == "create_repo" {
		data = f.repoJSON(f.newRepo(a))
	} else {
		r := f.repo(str(a["owner"]), str(a["repo"]))
		switch name {
		case "get_repo":
			data = f.repoJSON(r)
		case "create_issue":
			data = f.newIssue(r, a)
		case "list_issues":
			data = issueList(r, fallback(str(a["state"]), "open"), false)
		case "create_pull_request":
			data = f.newPull(r, a)
		case "merge_pull_request":
			data = f.mergePull(r, r.lookupPull(num(a["pull_number"])), a)
		}
	}
	return args{"content": []args{{"type": "text", "text": string(encode(data))}}}
}
