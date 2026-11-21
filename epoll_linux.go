package main

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"slices"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

const maxPolledBody = 64 << 10
const readInterest = syscall.EPOLLIN | syscall.EPOLLRDHUP

// Connections with standard length framing share the event loop. Complex HTTP
// exchanges move to the original connection handler with their input intact.
type pollConn struct {
	input           []byte
	parser          wireRequest
	bodySource      bytes.Reader
	body            wireBody
	headerSource    bytes.Reader
	headerReader    *bufio.Reader
	interest        uint32
	eof             bool
	output          bytes.Buffer
	writer          *bufio.Writer
	reply           wireResponse
	request         *http.Request
	headerEnd       int
	handled         int
	closeAfterWrite bool
	peerClosed      bool
	lastUsed        time.Time
}

func serveExperimental(s *wireServer, listener net.Listener) error {
	tcp, ok := listener.(*net.TCPListener)
	if !ok {
		return errors.New("epoll requires a TCP listener")
	}
	file, err := tcp.File()
	if err != nil {
		return err
	}
	defer file.Close()
	fd := int(file.Fd())
	if err := syscall.SetNonblock(fd, true); err != nil {
		return err
	}
	ep, err := syscall.EpollCreate1(syscall.EPOLL_CLOEXEC)
	if err != nil {
		return err
	}
	defer syscall.Close(ep)
	if err = syscall.EpollCtl(ep, syscall.EPOLL_CTL_ADD, fd, &syscall.EpollEvent{Events: syscall.EPOLLIN, Fd: int32(fd)}); err != nil {
		return err
	}
	connections := make(map[int]*pollConn)
	defer func() {
		for conn := range connections {
			_ = syscall.Close(conn)
		}
	}()
	closePeer := func(conn int) {
		_ = syscall.EpollCtl(ep, syscall.EPOLL_CTL_DEL, conn, nil)
		delete(connections, conn)
		_ = syscall.Close(conn)
	}
	var events [64]syscall.EpollEvent
	lastSweep := time.Now()
	var streams atomic.Int32
	for {
		count, err := syscall.EpollWait(ep, events[:], 200)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		s.mu.Lock()
		closing := s.closing
		s.mu.Unlock()
		if closing {
			return net.ErrClosed
		}
		now := time.Now()
		for _, ev := range events[:count] {
			conn := int(ev.Fd)
			if conn == fd {
				for i := 0; i < 64; i++ {
					peer, err := acceptPollSocket(fd)
					if err != 0 {
						break
					}
					if len(connections) >= 512 {
						_ = syscall.Close(peer)
						continue
					}
					_ = syscall.SetsockoptInt(peer, syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1)
					if err := syscall.EpollCtl(ep, syscall.EPOLL_CTL_ADD, peer, &syscall.EpollEvent{Events: readInterest, Fd: int32(peer)}); err != nil {
						_ = syscall.Close(peer)
						continue
					}
					c := &pollConn{lastUsed: now, interest: readInterest}
					c.headerReader = bufio.NewReader(&c.headerSource)
					c.writer = bufio.NewWriterSize(&c.output, 4096)
					c.reply.header = make(http.Header, 7)
					connections[peer] = c
				}
				continue
			}
			c := connections[conn]
			if c == nil {
				continue
			}
			c.lastUsed = now
			if ev.Events&(syscall.EPOLLHUP|syscall.EPOLLERR) != 0 {
				closePeer(conn)
				continue
			}
			if ev.Events&syscall.EPOLLRDHUP != 0 {
				c.peerClosed = true
			}
			if c.output.Len() == 0 && ev.Events&(syscall.EPOLLIN|syscall.EPOLLRDHUP) != 0 {
				start := len(c.input)
				c.input = slices.Grow(c.input, 4096)
				n, err := pollIO(syscall.SYS_READ, conn, c.input[start:start+4096])
				c.input = c.input[:start+n]
				if n == 0 && err == 0 {
					c.eof = true
				}
				if err != 0 && err != syscall.EAGAIN && err != syscall.EINTR {
					closePeer(conn)
					continue
				}
			}
			if c.output.Len() > 0 {
				if !c.flush(conn) {
					closePeer(conn)
					continue
				}
				if c.output.Len() == 0 && c.closeAfterWrite {
					closePeer(conn)
					continue
				}
			}
			if c.output.Len() == 0 {
				switch c.step(s) {
				case pollFallback:
					_ = syscall.EpollCtl(ep, syscall.EPOLL_CTL_DEL, conn, nil)
					delete(connections, conn)
					s.handoff(conn, c.input)
					continue
				case pollClose:
					closePeer(conn)
					continue
				}
				// Try the common one-write path. EPOLLOUT resumes a partial write.
				if c.output.Len() != 0 {
					if !c.flush(conn) {
						closePeer(conn)
						continue
					}
					if c.output.Len() == 0 && c.closeAfterWrite {
						closePeer(conn)
						continue
					}
				}
			}
			if c.output.Len() == 0 && (c.eof || c.peerClosed && len(c.input) == 0) && !c.hasCompleteInput() {
				closePeer(conn)
				continue
			}
			if c.handled >= 12 && c.output.Len() == 0 && len(c.input) == 0 && !c.peerClosed && !c.closeAfterWrite && streams.Load() < maxStreamPeers {
				stream, err := newStreamPeer(conn)
				if err == nil {
					_ = syscall.EpollCtl(ep, syscall.EPOLL_CTL_DEL, conn, nil)
					delete(connections, conn)
					streams.Add(1)
					s.acceptStream(stream, func() { streams.Add(-1) })
					continue
				}
			}
			interest := uint32(readInterest)
			if c.output.Len() > 0 || c.hasCompleteInput() {
				interest = syscall.EPOLLOUT
			}
			if interest != c.interest {
				if err := syscall.EpollCtl(ep, syscall.EPOLL_CTL_MOD, conn, &syscall.EpollEvent{Events: interest, Fd: int32(conn)}); err != nil {
					closePeer(conn)
					continue
				}
				c.interest = interest
			}
		}
		if now.Sub(lastSweep) >= time.Second {
			for peer, c := range connections {
				if now.Sub(c.lastUsed) > 5*time.Second {
					closePeer(peer)
				}
			}
			lastSweep = now
		}
	}
}

// These operations use nonblocking sockets. Keep readiness waits on the
// regular syscall path so the Go scheduler can run the control goroutine.
func acceptPollSocket(fd int) (int, syscall.Errno) {
	peer, _, err := syscall.RawSyscall6(syscall.SYS_ACCEPT4, uintptr(fd), 0, 0,
		syscall.SOCK_NONBLOCK|syscall.SOCK_CLOEXEC, 0, 0)
	return int(peer), err
}

func pollIO(operation uintptr, fd int, data []byte) (int, syscall.Errno) {
	n, _, err := syscall.RawSyscall(operation, uintptr(fd),
		uintptr(unsafe.Pointer(unsafe.SliceData(data))), uintptr(len(data)))
	runtime.KeepAlive(data)
	if err != 0 {
		return 0, err
	}
	return int(n), 0
}

func (c *pollConn) flush(fd int) bool {
	n, err := pollIO(syscall.SYS_WRITE, fd, c.output.Bytes())
	if n > 0 {
		c.output.Next(n)
	}
	return err == 0 || err == syscall.EAGAIN || err == syscall.EINTR
}

type pollStep uint8

const (
	pollWait pollStep = iota
	pollFallback
	pollClose
)

var headerEnd = []byte("\r\n\r\n")

func (c *pollConn) hasCompleteInput() bool {
	if c.request != nil {
		return int64(len(c.input)-c.headerEnd) >= c.request.ContentLength
	}
	return bytes.Contains(c.input, headerEnd)
}

func (c *pollConn) step(s *wireServer) pollStep {
	if c.request == nil {
		end := bytes.Index(c.input, headerEnd)
		if end < 0 {
			if len(c.input) >= httpReadLimit {
				return pollFallback
			}
			return pollWait
		}
		end += len(headerEnd)
		if end > httpReadLimit {
			return pollFallback
		}
		req := c.parser.parse(string(c.input[:end-len(headerEnd)]))
		var err error
		if req == nil {
			c.headerSource.Reset(c.input[:end])
			c.headerReader.Reset(&c.headerSource)
			req, err = http.ReadRequest(c.headerReader)
		}
		if err != nil || req.Proto != "HTTP/1.1" || req.Host == "" || len(req.TransferEncoding) > 0 || req.ContentLength < 0 ||
			req.ContentLength > maxPolledBody || req.Header.Get("Expect") != "" || req.Header.Get("Upgrade") != "" {
			return pollFallback
		}
		c.request, c.headerEnd = req, end
	}
	req := c.request
	end := c.headerEnd + int(req.ContentLength)
	if len(c.input) < end {
		return pollWait
	}
	if req.ContentLength > 0 {
		c.bodySource.Reset(c.input[c.headerEnd:end])
		c.parser.body.LimitedReader = io.LimitedReader{R: &c.bodySource, N: req.ContentLength}
		c.body = wireBody{ReadCloser: &c.parser.body, writer: c.writer}
		req.Body = &c.body
	}
	clear(c.reply.header)
	c.reply = wireResponse{header: c.reply.header, writer: c.writer, request: req, close: req.Close}
	if req.ContentLength > 0 {
		c.reply.body = &c.body
	}
	s.handle(&c.reply, req)
	if c.reply.broken {
		return pollClose
	}
	if !c.reply.started {
		send(&c.reply, 200, nil)
	}
	if err := c.writer.Flush(); err != nil {
		return pollClose
	}
	c.closeAfterWrite = c.reply.close
	c.handled++
	copy(c.input, c.input[end:])
	c.input = c.input[:len(c.input)-end]
	c.request = nil
	return pollWait
}

type prefixedConn struct {
	net.Conn
	prefix *bytes.Reader
}

func (c *prefixedConn) Read(out []byte) (int, error) {
	if c.prefix.Len() != 0 {
		return c.prefix.Read(out)
	}
	return c.Conn.Read(out)
}

func (s *wireServer) handoff(fd int, input []byte) {
	file := os.NewFile(uintptr(fd), "forge-http")
	conn, err := net.FileConn(file)
	_ = file.Close()
	if err != nil {
		return
	}
	prefixed := &prefixedConn{conn, bytes.NewReader(input)}
	s.mu.Lock()
	if s.closing || len(s.conns) >= 512 {
		s.mu.Unlock()
		_ = conn.Close()
		return
	}
	if s.conns == nil {
		s.conns = make(map[net.Conn]struct{})
	}
	s.conns[prefixed] = struct{}{}
	s.mu.Unlock()
	go s.serveConn(prefixed)
}
