package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

type gqlValue func(args) any
type gqlFields map[string]gqlValue
type gqlDirective struct {
	name    string
	options gqlFields
}
type gqlSelection struct {
	name, alias, on, fragment string
	options                   gqlFields
	directives                []gqlDirective
	children                  []gqlSelection
}
type gqlOperation struct {
	kind, name string
	defaults   gqlFields
	children   []gqlSelection
}
type gqlFragment struct {
	on       string
	children []gqlSelection
}
type gqlParser struct {
	tokens       []string
	index, depth int
	fragments    map[string]gqlFragment
}

var gqlToken = regexp.MustCompile(`^(?:#[^\r\n]*|[\s,]+|\.\.\.|[_A-Za-z][_A-Za-z0-9]*|-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?|"(?:[^"\\\r\n]|\\.)*"|[!$():=@\[\]{|}])`)
var gqlName = regexp.MustCompile(`^[_A-Za-z][_A-Za-z0-9]*$`)

func gqlError(message string) { fail(400, message) }
func parseGraphQL(source string) (*gqlParser, []gqlOperation) {
	if len(source) == 0 || len(source) > 512<<10 {
		gqlError("Expected a GraphQL query")
	}
	p := &gqlParser{fragments: make(map[string]gqlFragment)}
	for len(source) > 0 {
		s := gqlToken.FindString(source)
		if s == "" {
			gqlError("Invalid GraphQL token")
		}
		switch s[0] {
		case '#', ' ', '\t', '\n', '\r', ',', '\f':
		default:
			p.tokens = append(p.tokens, s)
		}
		if len(p.tokens) > 16000 {
			gqlError("Query has too many tokens")
		}
		source = source[len(s):]
	}
	var ops []gqlOperation
	for p.peek() != "" {
		if p.take("fragment") {
			name := p.name()
			p.expect("on")
			on := p.name()
			p.directives()
			p.fragments[name] = gqlFragment{on, p.selectionSet()}
			continue
		}
		op := gqlOperation{kind: "query", defaults: make(gqlFields)}
		if p.peek() != "{" {
			op.kind = p.name()
			if op.kind != "query" && op.kind != "mutation" {
				gqlError("Unsupported operation")
			}
			if gqlName.MatchString(p.peek()) {
				op.name = p.name()
			}
		}
		if p.take("(") {
			for !p.take(")") {
				p.expect("$")
				key := p.name()
				p.expect(":")
				p.variableType(0)
				if p.take("=") {
					op.defaults[key] = p.value()
				}
				p.directives()
			}
		}
		p.directives()
		op.children = p.selectionSet()
		ops = append(ops, op)
	}
	return p, ops
}
func (p *gqlParser) peek() string {
	if p.index == len(p.tokens) {
		return ""
	}
	return p.tokens[p.index]
}
func (p *gqlParser) take(token string) bool {
	if p.peek() != token {
		return false
	}
	p.index++
	return true
}
func (p *gqlParser) next() string {
	s := p.peek()
	if s == "" {
		gqlError("Unexpected end of query")
	}
	p.index++
	return s
}
func (p *gqlParser) expect(token string) {
	if !p.take(token) {
		gqlError("Expected " + token + " in query")
	}
}
func (p *gqlParser) name() string {
	s := p.next()
	if !gqlName.MatchString(s) {
		gqlError("Expected GraphQL name")
	}
	return s
}
func (p *gqlParser) enter() {
	p.depth++
	if p.depth > 60 {
		gqlError("Query nested too deeply")
	}
}
func (p *gqlParser) variableType(d int) {
	if d > 60 {
		gqlError("Variable type nested too deeply")
	}
	if p.take("[") {
		p.variableType(d + 1)
		p.expect("]")
	} else {
		p.name()
	}
	p.take("!")
}
func (p *gqlParser) value() gqlValue {
	p.enter()
	defer func() { p.depth-- }()
	switch {
	case p.take("$"):
		key := p.name()
		return func(v args) any { return v[key] }
	case p.take("["):
		var items []gqlValue
		for !p.take("]") {
			items = append(items, p.value())
		}
		return func(v args) any {
			out := make([]any, len(items))
			for i, item := range items {
				out[i] = item(v)
			}
			return out
		}
	case p.take("{"):
		opts := p.arguments("}")
		return func(v args) any { return evalArgs(opts, v) }
	}
	token := p.next()
	var val any
	if token == "true" || token == "false" || token == "null" || token[0] == '"' || token[0] == '-' || (token[0] >= '0' && token[0] <= '9') {
		if json.Unmarshal([]byte(token), &val) != nil {
			gqlError("Invalid GraphQL literal")
		}
	} else if gqlName.MatchString(token) {
		val = token
	} else {
		gqlError("Invalid GraphQL literal")
	}
	return func(args) any { return val }
}
func (p *gqlParser) arguments(end string) gqlFields {
	result := make(gqlFields)
	for !p.take(end) {
		key := p.name()
		p.expect(":")
		result[key] = p.value()
	}
	return result
}
func (p *gqlParser) directives() []gqlDirective {
	var rows []gqlDirective
	for p.take("@") {
		d := gqlDirective{name: p.name()}
		if p.take("(") {
			d.options = p.arguments(")")
		}
		rows = append(rows, d)
	}
	return rows
}
func (p *gqlParser) selectionSet() []gqlSelection {
	p.expect("{")
	p.enter()
	defer func() { p.depth-- }()
	var rows []gqlSelection
	for !p.take("}") {
		var s gqlSelection
		if p.take("...") {
			if p.take("on") {
				s.on = p.name()
			} else if p.peek() != "@" && p.peek() != "{" {
				s.fragment = p.name()
			}
			s.directives = p.directives()
			if s.fragment == "" {
				s.children = p.selectionSet()
			}
		} else {
			s.alias = p.name()
			s.name = s.alias
			if p.take(":") {
				s.name = p.name()
			}
			if p.take("(") {
				s.options = p.arguments(")")
			}
			s.directives = p.directives()
			if p.peek() == "{" {
				s.children = p.selectionSet()
			}
		}
		rows = append(rows, s)
	}
	if len(rows) == 0 {
		gqlError("Empty GraphQL selection")
	}
	return rows
}
func evalArgs(opts gqlFields, vars args) args {
	result := make(args, len(opts))
	for k, v := range opts {
		result[k] = v(vars)
	}
	return result
}

type graphNode struct {
	kind   string
	fields args
}
type resolver func(args) any

func node(kind string, fields args) *graphNode { return &graphNode{kind, fields} }
func resolve(field func(args) any) resolver    { return field }

type projection struct {
	vars      args
	fragments map[string]gqlFragment
	active    map[string]bool
	budget    int
}

func (p *projection) project(value any, selections []gqlSelection) any {
	if value == nil {
		return nil
	}
	switch v := value.(type) {
	case []*graphNode:
		result := make([]any, len(v))
		for i, child := range v {
			result[i] = p.project(child, selections)
		}
		return result
	case *graphNode:
		if v == nil {
			return nil
		}
		if len(selections) == 0 {
			gqlError("Field requires a selection set")
		}
		out := args{}
		for _, s := range selections {
			p.budget--
			if p.budget < 0 {
				gqlError("Query exceeds result budget")
			}
			if slices.ContainsFunc(s.directives, func(d gqlDirective) bool {
				v := d.options["if"]
				if v == nil {
					return false
				}
				on, ok := v(p.vars).(bool)
				return ok && ((d.name == "skip" && on) || (d.name == "include" && !on))
			}) {
				continue
			}
			if s.name == "" {
				on, children := s.on, s.children
				if s.fragment != "" {
					f, ok := p.fragments[s.fragment]
					if !ok || p.active[s.fragment] {
						gqlError("Invalid or cyclic fragment " + s.fragment)
					}
					on, children = f.on, f.children
				}
				if on != "" && on != v.kind && !(on == "Node" && v.fields["id"] != nil) {
					continue
				}
				if s.fragment != "" {
					p.active[s.fragment] = true
				}
				for k, item := range p.project(v, children).(args) {
					out[k] = item
				}
				delete(p.active, s.fragment)
				continue
			}
			if s.name == "__typename" {
				out[s.alias] = v.kind
				continue
			}
			child, ok := v.fields[s.name]
			if !ok {
				gqlError(fmt.Sprintf("Cannot query field %q on type %q", s.name, v.kind))
			}
			if get, ok := child.(resolver); ok {
				child = get(evalArgs(s.options, p.vars))
			}
			out[s.alias] = p.project(child, s.children)
		}
		return out
	default:
		if len(selections) != 0 {
			gqlError("A scalar cannot have a selection set")
		}
		return value
	}
}
func connection(kind string, items []*graphNode, options args) *graphNode {
	first := 30
	if v, ok := options["first"]; ok && v != nil {
		first = min(100, max(0, num(v)))
	}
	cursor := func(i int) string { return base64.RawURLEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(i))) }
	var start int
	if marker, e := base64.RawURLEncoding.DecodeString(str(options["after"])); e == nil {
		if text, ok := strings.CutPrefix(string(marker), "cursor:"); ok {
			if n, e := strconv.Atoi(text); e == nil && n >= 0 {
				start = min(n, len(items)) + 1
			}
		}
	}
	start = min(len(items), start)
	end := min(start+first, len(items))
	nodes := items[start:end]
	var startCursor, endCursor any
	if len(nodes) > 0 {
		startCursor = cursor(start)
		endCursor = cursor(end - 1)
	}
	edges := make([]*graphNode, len(nodes))
	for i, v := range nodes {
		edges[i] = node(kind+"Edge", args{"node": v, "cursor": cursor(i + start)})
	}
	return node(kind+"Connection", args{"totalCount": len(items), "nodes": nodes, "edges": edges, "pageInfo": node("PageInfo", args{"hasNextPage": end < len(items), "hasPreviousPage": start > 0, "startCursor": startCursor, "endCursor": endCursor})})
}
func (f *forge) executeGraphQL(value any) any {
	if rows, ok := value.([]any); ok {
		result := make([]any, len(rows))
		for i, row := range rows {
			result[i] = f.gqlOne(obj(row))
		}
		return result
	}
	return f.gqlOne(obj(value))
}
func (f *forge) gqlOne(a args) (out any) {
	defer func() {
		if e := recover(); e != nil {
			if err, ok := e.(apiFault); ok {
				out = args{"errors": []args{{"message": err.message}}}
			} else {
				panic(e)
			}
		}
	}()
	p, ops := parseGraphQL(str(a["query"]))
	name := str(a["operationName"])
	var operation *gqlOperation
	if name == "" && len(ops) == 1 {
		operation = &ops[0]
	} else {
		for i := range ops {
			if name != "" && ops[i].name == name {
				operation = &ops[i]
				break
			}
		}
	}
	if operation == nil {
		gqlError("Select a named GraphQL operation")
	}
	provided := obj(a["variables"])
	vars := evalArgs(operation.defaults, provided)
	for k, v := range provided {
		vars[k] = v
	}
	var root *graphNode
	if operation.kind == "mutation" {
		root = f.mutationGraph()
	} else {
		root = f.queryGraph()
	}
	projector := projection{vars, p.fragments, make(map[string]bool), 50000}
	return args{"data": projector.project(root, operation.children)}
}
