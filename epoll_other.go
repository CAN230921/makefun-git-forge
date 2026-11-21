//go:build !linux

package main

import "net"

func serveExperimental(s *wireServer, listener net.Listener) error {
	return s.Serve(listener)
}
