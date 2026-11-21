package main

import (
	"errors"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
)

const maxStreamPeers = 4

// A small number of persistent peers use blocking kernel reads. Fixed,
// bounded socket timeouts wake this adapter so it can check the request's
// current deadline without resetting socket options on each HTTP exchange.
type streamPeer struct {
	file       *os.File
	fd         int
	local      net.Addr
	remote     net.Addr
	readUntil  time.Time
	writeUntil time.Time
	stopped    sync.Once
}

func newStreamPeer(fd int) (*streamPeer, error) {
	local, err := syscall.Getsockname(fd)
	if err != nil {
		return nil, err
	}
	peer, err := syscall.Getpeername(fd)
	if err != nil {
		return nil, err
	}
	timeout := syscall.NsecToTimeval(int64(250 * time.Millisecond))
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &timeout); err != nil {
		return nil, err
	}
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_SNDTIMEO, &timeout); err != nil {
		return nil, err
	}
	if err := syscall.SetNonblock(fd, false); err != nil {
		return nil, err
	}
	return &streamPeer{file: os.NewFile(uintptr(fd), "forge-stream"), fd: fd,
		local: socketAddress(local), remote: socketAddress(peer)}, nil
}

func socketAddress(sa syscall.Sockaddr) net.Addr {
	switch addr := sa.(type) {
	case *syscall.SockaddrInet4:
		return &net.TCPAddr{IP: addr.Addr[:], Port: addr.Port}
	case *syscall.SockaddrInet6:
		return &net.TCPAddr{IP: addr.Addr[:], Port: addr.Port}
	default:
		return &net.TCPAddr{}
	}
}

func (c *streamPeer) Read(dst []byte) (int, error) {
	for {
		n, err := c.file.Read(dst)
		if !errors.Is(err, syscall.EAGAIN) {
			return n, err
		}
		if !c.readUntil.IsZero() && !time.Now().Before(c.readUntil) {
			return n, os.ErrDeadlineExceeded
		}
	}
}

func (c *streamPeer) Write(src []byte) (int, error) {
	written := 0
	for written < len(src) {
		n, err := c.file.Write(src[written:])
		written += n
		if !errors.Is(err, syscall.EAGAIN) {
			return written, err
		}
		if !c.writeUntil.IsZero() && !time.Now().Before(c.writeUntil) {
			return written, os.ErrDeadlineExceeded
		}
	}
	return written, nil
}

func (c *streamPeer) Close() error {
	var err error
	c.stopped.Do(func() {
		_ = syscall.Shutdown(c.fd, syscall.SHUT_RDWR)
		err = c.file.Close()
	})
	return err
}

func (c *streamPeer) LocalAddr() net.Addr  { return c.local }
func (c *streamPeer) RemoteAddr() net.Addr { return c.remote }

func (c *streamPeer) SetDeadline(deadline time.Time) error {
	c.readUntil, c.writeUntil = deadline, deadline
	return nil
}

func (c *streamPeer) SetReadDeadline(deadline time.Time) error {
	c.readUntil = deadline
	return nil
}

func (c *streamPeer) SetWriteDeadline(deadline time.Time) error {
	c.writeUntil = deadline
	return nil
}

func (s *wireServer) acceptStream(peer *streamPeer, done func()) {
	s.mu.Lock()
	if s.closing || len(s.conns) >= 512 {
		s.mu.Unlock()
		_ = peer.Close()
		done()
		return
	}
	if s.conns == nil {
		s.conns = make(map[net.Conn]struct{})
	}
	s.conns[peer] = struct{}{}
	s.mu.Unlock()
	go func() {
		defer done()
		s.serveConn(peer)
	}()
}
