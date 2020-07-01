package http1

import "net"

type Conn interface {
	Bytes() ([]byte, error)
	Shift(n int)
	Buffered() int
	Write(p []byte) (n int, err error)
	RemoteAddr() net.Addr
	Close() error
}
