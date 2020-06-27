package http1

type HandlerFunc func(ctx *Context)
type Server struct {
	Handler              HandlerFunc
	MaxServeTimesPerConn uint64
}

func NewServer(handler HandlerFunc, maxServeTimesPerConn uint64) *Server {
	return &Server{
		Handler:              handler,
		MaxServeTimesPerConn: maxServeTimesPerConn,
	}
}
