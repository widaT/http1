package http1

type Conn interface {
	Bytes() []byte
	Shift(n int)
	Buffered() int
}
