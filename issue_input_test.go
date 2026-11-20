package main

import (
	"encoding/json"
	"testing"
)

func TestSimpleIssueInput(t *testing.T) {
	for _, source := range []string{
		`{"title":"中文","body":"ok"}`,
		`{"body":"quote \" and slash \\", "tit\u006ce": "x"}`,
		"{\n\t\"title\" : \"x\"\r }\t",
		`{"title":"\ud800","body":"💡"}`,
	} {
		title, body, ok := simpleIssueInput([]byte(source))
		var generic map[string]any
		if err := json.Unmarshal([]byte(source), &generic); err != nil || !ok {
			t.Fatalf("%s: fast=%v, json=%v", source, ok, err)
		}
		if title != str(generic["title"]) || body != str(generic["body"]) {
			t.Fatalf("%s: got %q and %q", source, title, body)
		}
	}
	for _, source := range []string{`{"title":"x","title":null}`, `{"Title":"x"}`, `{"title":"x","labels":[]}`, `{"title":"x",}`, `{"title":"\u_xyz"}`, `{"title":"a"}{"title":"b"}`, "[]", "{}"} {
		if _, _, ok := simpleIssueInput([]byte(source)); ok {
			t.Fatalf("should fall back: %s", source)
		}
	}
}
