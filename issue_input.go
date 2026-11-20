package main

import (
	"bytes"
	"encoding/json"
	"unicode/utf8"
)

func trimJSONSpace(data []byte) []byte {
	return bytes.TrimLeft(data, " \n\r\t")
}

// simpleIssueInput accepts a JSON object containing unique title/body string keys.
// The caller uses the general decoder for all other shapes, including labels.
func simpleIssueInput(data []byte) (title, body string, ok bool) {
	data = trimJSONSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return "", "", false
	}
	data = trimJSONSpace(data[1:])
	var seenTitle, seenBody bool
	for len(data) != 0 {
		key, rest, plain := readIssueString(data)
		if !plain {
			return "", "", false
		}
		data = trimJSONSpace(rest)
		if len(data) == 0 || data[0] != ':' {
			return "", "", false
		}
		value, rest, valid := readIssueString(trimJSONSpace(data[1:]))
		if !valid {
			return "", "", false
		}
		switch key {
		case "title":
			if seenTitle {
				return "", "", false
			}
			title, seenTitle = value, true
		case "body":
			if seenBody {
				return "", "", false
			}
			body, seenBody = value, true
		default:
			return "", "", false
		}
		data = trimJSONSpace(rest)
		if len(data) == 0 {
			return "", "", false
		}
		if data[0] == '}' {
			return title, body, seenTitle && len(trimJSONSpace(data[1:])) == 0
		}
		if data[0] != ',' {
			return "", "", false
		}
		data = trimJSONSpace(data[1:])
	}
	return "", "", false
}

func readIssueString(data []byte) (string, []byte, bool) {
	if len(data) == 0 || data[0] != '"' {
		return "", nil, false
	}
	escaped := false
	for i := 1; i < len(data); i++ {
		switch b := data[i]; {
		case b < 0x20:
			return "", nil, false
		case b == '\\':
			escaped = true
			i++
		case b == '"':
			var value string
			if !escaped && utf8.Valid(data[1:i]) {
				value = string(data[1:i])
			} else if json.Unmarshal(data[:i+1], &value) != nil {
				return "", nil, false
			}
			return value, data[i+1:], true
		}
	}
	return "", nil, false
}
