package main

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

var wireHeaderNames = [...]string{
	"Host", "Authorization", "Git-Protocol", "Content-Length", "Content-Type",
	"Connection", "Accept", "Accept-Encoding", "Accept-Language", "Sec-Fetch-Mode",
	"User-Agent", "Cache-Control", "X-Github-Api-Version",
}

// A request and its headers belong to the connection until its synchronous
// handler returns. Only accept complete, conventionally framed requests here.
type wireRequest struct {
	request http.Request
	target  url.URL
	headers http.Header
	values  [len(wireHeaderNames)]string
	body    fixedWireBody
}

type fixedWireBody struct {
	io.LimitedReader
}

func (b *fixedWireBody) Read(dst []byte) (int, error) {
	n, err := b.LimitedReader.Read(dst)
	if err == io.EOF && b.N > 0 {
		err = io.ErrUnexpectedEOF
	}
	if err == nil && b.N == 0 {
		err = io.EOF
	}
	return n, err
}

func (b *fixedWireBody) Close() error { return nil }

func plainTarget(target string, query bool) bool {
	for i := 0; i < len(target); i++ {
		c := target[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			continue
		}
		switch c {
		case '/', '.', '-', '_', '~':
			continue
		case '=', '&':
			if query {
				continue
			}
		}
		return false
	}
	return true
}

// Do not consume bytes until the fast path has validated the entire header.
// The standard parser handles fragmented headers, encoded paths, HTTP/1.0,
// trailers, duplicate fields, and every other shape outside the subset.
func (p *wireRequest) read(buffer *bufio.Reader) (*http.Request, error) {
	if _, err := buffer.Peek(1); err != nil {
		return nil, err
	}
	data, _ := buffer.Peek(buffer.Buffered())
	end := bytes.Index(data, []byte("\r\n\r\n"))
	if end < 0 {
		return http.ReadRequest(buffer)
	}
	size := end + 4
	if request := p.parse(string(data[:end])); request != nil {
		_, _ = buffer.Discard(size)
		if request.ContentLength > 0 {
			p.body.LimitedReader = io.LimitedReader{R: buffer, N: request.ContentLength}
			request.Body = &p.body
		}
		return request, nil
	}
	return http.ReadRequest(buffer)
}

func (p *wireRequest) parse(head string) *http.Request {
	line, headers, ok := strings.Cut(head, "\r\n")
	if !ok {
		return nil
	}
	method, rest, _ := strings.Cut(line, " ")
	requestURI, version, _ := strings.Cut(rest, " ")
	if (method != "GET" && method != "POST") || version != "HTTP/1.1" {
		return nil
	}
	path, query, hasQuery := strings.Cut(requestURI, "?")
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") ||
		!plainTarget(path, false) || !plainTarget(query, true) {
		return nil
	}
	if p.headers == nil {
		p.headers = make(http.Header, len(wireHeaderNames))
	} else {
		clear(p.headers)
		clear(p.values[:])
	}
	var seen uint16
	var host string
	var length int64
	var closeConn bool
	for headers != "" {
		line, headers, _ = strings.Cut(headers, "\r\n")
		key, raw, ok := strings.Cut(line, ":")
		if !ok {
			return nil
		}
		n := 0
		for n < len(wireHeaderNames) && !strings.EqualFold(key, wireHeaderNames[n]) {
			n++
		}
		if n == len(wireHeaderNames) || seen&(1<<n) != 0 {
			return nil
		}
		seen |= 1 << n
		value := strings.Trim(raw, " \t")
		for i := 0; i < len(value); i++ {
			if (value[i] < 32 && value[i] != '\t') || value[i] >= 127 {
				return nil
			}
		}
		switch wireHeaderNames[n] {
		case "Host":
			if value == "" || strings.ContainsAny(value, " \t,/@?#\\") {
				return nil
			}
			host = value
			continue
		case "Content-Length":
			// ParseInt also accepts signs. Use the same digits-only framing
			// as the standard parser before interpreting the length.
			for i := 0; i < len(value); i++ {
				if value[i] < '0' || value[i] > '9' {
					return nil
				}
			}
			var err error
			length, err = strconv.ParseInt(value, 10, 64)
			if err != nil {
				return nil
			}
		case "Connection":
			closeConn = strings.EqualFold(value, "close")
			if !closeConn && !strings.EqualFold(value, "keep-alive") {
				return nil
			}
		}
		p.values[n] = value
		p.headers[wireHeaderNames[n]] = p.values[n : n+1 : n+1]
	}
	if host == "" {
		return nil
	}
	p.target = url.URL{Path: path, RawQuery: query, ForceQuery: hasQuery && query == ""}
	p.request = http.Request{
		Method: method, URL: &p.target, Host: host, RequestURI: requestURI,
		Proto: version, ProtoMajor: 1, ProtoMinor: 1,
		Body: http.NoBody, ContentLength: length, Header: p.headers, Close: closeConn,
	}
	return &p.request
}
