package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

type benchmarkConn struct {
	reader        *strings.Reader
	readRemaining int
}

const wireInput = "GET /api/v3/repos/test/sample/issues/1 HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer test\r\nAccept: application/json\r\n\r\n"

func (c *benchmarkConn) Read(out []byte) (int, error) {
	if c.reader.Len() == 0 {
		if c.readRemaining == 0 {
			return 0, io.EOF
		}
		c.readRemaining--
		c.reader.Reset(wireInput)
	}
	return c.reader.Read(out)
}
func (*benchmarkConn) Write(buf []byte) (int, error) { return len(buf), nil }
func (*benchmarkConn) Close() error                  { return nil }
func (*benchmarkConn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8081}
}
func (*benchmarkConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 55883}
}
func (*benchmarkConn) SetDeadline(time.Time) error      { return nil }
func (*benchmarkConn) SetReadDeadline(time.Time) error  { return nil }
func (*benchmarkConn) SetWriteDeadline(time.Time) error { return nil }

func BenchmarkWireIssueConnection(b *testing.B) {
	f := &forge{bearer: "Bearer test", second: 1735689600, repos: make(map[string]*repository)}
	owner := "test"
	f.owner.Store(&owner)
	r := f.newRepo(args{"name": "sample"})
	f.newIssue(r, args{"title": "Read issue", "body": "Cached response"})
	server := &wireServer{handler: f}
	conn := &benchmarkConn{reader: strings.NewReader(""), readRemaining: b.N}
	b.ReportAllocs()
	b.ResetTimer()
	server.serveConn(conn)
}

func TestWireRespondsToClose(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	f := &forge{repos: make(map[string]*repository)}
	wire := &wireServer{handler: f}
	go wire.serveConn(server)
	if err := client.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	fmt.Fprint(client, "GET /healthz HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")
	got, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(got.Body)
	if err != nil || got.StatusCode != 200 || !bytes.Equal(body, []byte(`{"ok":true}`)) {
		t.Fatalf("reply: %v %s %v", got.StatusCode, body, err)
	}
}
