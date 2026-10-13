package plugin

import (
	"io"
	"net"

	pub "github.com/majorcontext/harness/plugin"
)

// InProcess returns the Spec of p, served in this process over an in-memory
// pipe. Each serve loop runs through spawn, which fails when the owner is closed.
func InProcess(p *pub.Plugin, spawn func(func()) error) Spec {
	return Spec{Manifest: p.Manifest(), dial: func() (io.ReadWriteCloser, error) {
		client, server := net.Pipe()
		if err := spawn(func() { _ = pub.ServeConn(server, p) }); err != nil {
			_ = client.Close()
			_ = server.Close()
			return nil, err
		}
		return client, nil
	}}
}
