package main

import (
	"bufio"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const httpReadLimit = 16 << 10

// Keep the standard HTTP request parser and stream framing. Each connection
// owns two buffers for its lifetime and emits a response with one flush.
type wireServer struct {
	handler http.Handler
	mu      sync.Mutex
	closing bool
	conns   map[net.Conn]struct{}
}

type wireReader struct {
	io.Reader
	headerLimit int
	limited     bool
}

var errHeaderLimit = errors.New("request headers too large")

func (r *wireReader) Read(out []byte) (int, error) {
	if r.limited {
		if r.headerLimit == 0 {
			return 0, errHeaderLimit
		}
		out = out[:min(len(out), r.headerLimit)]
	}
	n, err := r.Reader.Read(out)
	if r.limited {
		r.headerLimit -= n
	}
	return n, err
}

type wireBody struct {
	io.ReadCloser
	done   bool
	writer *bufio.Writer
	expect bool
}

func (b *wireBody) Read(out []byte) (int, error) {
	if b.expect {
		b.expect = false
		if _, err := b.writer.WriteString("HTTP/1.1 100 Continue\r\n\r\n"); err != nil {
			return 0, err
		}
		if err := b.writer.Flush(); err != nil {
			return 0, err
		}
	}
	n, err := b.ReadCloser.Read(out)
	if err == io.EOF {
		b.done = true
	}
	return n, err
}

type wireDate struct {
	second int64
	line   string
}

var cachedWireDate atomic.Pointer[wireDate]

func responseDate() string {
	now := time.Now()
	if previous := cachedWireDate.Load(); previous != nil && previous.second == now.Unix() {
		return previous.line
	}
	date := &wireDate{now.Unix(), "Date: " + now.UTC().Format(http.TimeFormat) + "\r\n"}
	cachedWireDate.Store(date)
	return date.line
}

type wireResponse struct {
	header                           http.Header
	writer                           *bufio.Writer
	request                          *http.Request
	body                             *wireBody
	started, suppress, close, broken bool
}

func (w *wireResponse) Header() http.Header { return w.header }

func (w *wireResponse) WriteHeader(status int) {
	if w.started {
		return
	}
	w.started = true
	w.suppress = w.request.Method == "HEAD" || status < 200 || status == 204 || status == 304
	proto := "HTTP/1.1"
	if w.request.ProtoMinor == 0 {
		proto = "HTTP/1.0"
	}
	if !w.suppress && w.header.Get("Content-Length") == "" {
		w.close = true
	}
	if w.body != nil && !w.body.done {
		w.close = true
	}
	written := w.writer
	_, _ = written.WriteString(proto + " " + strconv.Itoa(status) + " " + http.StatusText(status) + "\r\n")
	if w.close {
		_, _ = written.WriteString("Connection: close\r\n")
	} else if w.request.ProtoMinor == 0 {
		_, _ = written.WriteString("Connection: keep-alive\r\n")
	}
	if w.header.Get("Date") == "" {
		_, _ = written.WriteString(responseDate())
	}
	for name, values := range w.header {
		if (status == 204 && name == "Content-Length") || name == "Connection" {
			continue
		}
		for _, v := range values {
			if strings.ContainsAny(v, "\r\n") {
				continue
			}
			_, _ = written.WriteString(name)
			_, _ = written.WriteString(": ")
			_, _ = written.WriteString(v)
			_, _ = written.WriteString("\r\n")
		}
	}
	_, _ = written.WriteString("\r\n")
}

func (w *wireResponse) writeIssue(data []byte, requestID uint64) {
	w.started = true
	proto := "HTTP/1.1"
	if w.request.ProtoMinor == 0 {
		proto = "HTTP/1.0"
	}
	out := w.writer
	_, _ = out.WriteString(proto + " 200 OK\r\nContent-Type: application/json; charset=utf-8\r\nX-GitHub-Request-Id: ")
	var digits [24]byte
	_, _ = out.Write(strconv.AppendUint(digits[:0], requestID, 16))
	_, _ = out.WriteString("\r\nContent-Length: ")
	_, _ = out.Write(strconv.AppendInt(digits[:0], int64(len(data)), 10))
	_, _ = out.WriteString("\r\n")
	_, _ = out.WriteString(responseDate())
	if w.close {
		_, _ = out.WriteString("Connection: close\r\n")
	} else if w.request.ProtoMinor == 0 {
		_, _ = out.WriteString("Connection: keep-alive\r\n")
	}
	_, _ = out.WriteString("\r\n")
	_, _ = out.Write(data)
}

func (w *wireResponse) Write(data []byte) (int, error) {
	if !w.started {
		w.WriteHeader(200)
	}
	if w.suppress {
		return len(data), nil
	}
	return w.writer.Write(data)
}

func (s *wireServer) handle(w *wireResponse, r *http.Request) {
	defer func() {
		if failure := recover(); failure != nil {
			w.broken = true
			log.Printf("HTTP request aborted: %v", failure)
		}
	}()
	s.handler.ServeHTTP(w, r)
}

func (s *wireServer) serveConn(c net.Conn) {
	defer func() {
		_ = c.Close()
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
	}()
	reader := wireReader{Reader: c}
	buffer := bufio.NewReaderSize(&reader, 4096)
	writer := bufio.NewWriterSize(c, 4096)
	headers := make(http.Header, 7)
	remote := c.RemoteAddr().String()
	var body wireBody
	var reply wireResponse
	for {
		if c.SetReadDeadline(time.Now().Add(5*time.Second)) != nil {
			return
		}
		reader.headerLimit, reader.limited = httpReadLimit, true
		request, err := http.ReadRequest(buffer)
		reader.limited = false
		if err != nil {
			if !errors.Is(err, io.EOF) {
				var timeout net.Error
				if errors.As(err, &timeout) && timeout.Timeout() {
					return
				}
				status := 400
				if errors.Is(err, errHeaderLimit) {
					status = 431
				}
				_ = c.SetWriteDeadline(time.Now().Add(time.Second))
				_, _ = writer.WriteString("HTTP/1.1 " + strconv.Itoa(status) + " " + http.StatusText(status) + "\r\nConnection: close\r\nContent-Length: 0\r\n\r\n")
				_ = writer.Flush()
			}
			return
		}
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
		request.RemoteAddr = remote
		clear(headers)
		unsupported := request.ProtoMajor != 1 || request.ProtoMinor > 1
		expect := request.Header.Get("Expect")
		badExpectation := expect != "" && !strings.EqualFold(expect, "100-continue")
		reply = wireResponse{writer: writer, header: headers, request: request, close: request.Close || unsupported || badExpectation}
		if request.Body != http.NoBody {
			body = wireBody{ReadCloser: request.Body, writer: writer, expect: expect != "" && !badExpectation}
			reply.body = &body
			request.Body = &body
		}
		switch {
		case unsupported:
			problem(&reply, 505, "HTTP Version Not Supported")
		case badExpectation:
			problem(&reply, 417, "Expectation Failed")
		case request.ProtoMinor == 1 && request.Host == "":
			problem(&reply, 400, "Host is required")
			reply.close = true
		default:
			s.handle(&reply, request)
		}
		if reply.broken {
			return
		}
		if !reply.started {
			send(&reply, 200, nil)
		}
		if writer.Flush() != nil || reply.close {
			return
		}
		if request.Body != http.NoBody && request.Body.Close() != nil {
			return
		}
	}
}

func (s *wireServer) Serve(listener net.Listener) error {
	for {
		c, err := listener.Accept()
		if err != nil {
			return err
		}
		s.mu.Lock()
		if s.closing || len(s.conns) >= 512 {
			s.mu.Unlock()
			_ = c.Close()
			continue
		}
		if s.conns == nil {
			s.conns = make(map[net.Conn]struct{})
		}
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		go s.serveConn(c)
	}
}

func (s *wireServer) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closing = true
	for c := range s.conns {
		_ = c.Close()
	}
}
