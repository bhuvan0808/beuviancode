package http

import (
	"fmt"
	"net"
)

// listen binds the address synchronously.
//
// Separated from Start so a port conflict surfaces as a startup error the
// supervisor can report against a named component. Handing the address to Fiber
// and letting it bind inside a goroutine would turn "port already in use" into a
// log line after the process had already announced itself as started.
func listen(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("http: cannot bind %s: %w", addr, err)
	}
	return ln, nil
}
