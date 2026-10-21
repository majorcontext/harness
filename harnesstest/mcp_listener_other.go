//go:build !linux

package harnesstest

import (
	"math/rand/v2"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
)

const (
	mcpPortBase = 20000
	mcpPortSpan = 10000
)

var (
	mcpNextPort  atomic.Uint32
	mcpPortStart = sync.OnceFunc(func() { mcpNextPort.Store(rand.Uint32N(mcpPortSpan)) })
)

// Sequential ports below the default ephemeral range prevent bind(0) from reusing a closed fixture address.
// A widened ephemeral range or a wildcard listener on macOS can break that isolation.
func listenMCP() (net.Listener, func(), error) {
	mcpPortStart()
	var err error
	for range 200 {
		var ln net.Listener
		ln, err = net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(mcpPortBase+int(mcpNextPort.Add(1)%mcpPortSpan)))
		if err == nil {
			return ln, func() {}, nil
		}
	}
	return nil, nil, err
}
