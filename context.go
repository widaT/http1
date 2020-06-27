package http1

import (
	"bufio"
	"net"
	"sync"

	"github.com/pkg/errors"
)

type Context struct {
	conn            Conn
	s               *Server
	continueReqSend bool
	req             *Request
	resp            *Response
	connRequestNum  uint64
	writer          *bufio.Writer
}

func NewContext(s *Server, conn Conn) *Context {
	return &Context{
		s:      s,
		conn:   conn,
		req:    NewRequst(conn.RemoteAddr().String()),
		resp:   NewResponse(),
		writer: bufio.NewWriterSize(conn, 4096),
	}
}

func (ctx *Context) Reset(conn Conn) {
	ctx.conn = conn
	ctx.resp.Reset()
	ctx.req.Reset()
	ctx.connRequestNum = 0
	ctx.continueReqSend = false
	ctx.writer.Reset(conn)
}

func (ctx *Context) RemoteAddr() net.Addr {
	return ctx.conn.RemoteAddr()
}

func (ctx *Context) Request() *Request {
	return ctx.req
}

func (ctx *Context) Response() *Response {
	return ctx.resp
}

func (ctx *Context) ServeHttp() error {
	if !ctx.req.parseHeaderComplete {
		if err := ctx.req.parse(ctx.conn); err != nil {
			return err
		}
	}
	//100 continue
	if ctx.req.IsContinue() && !ctx.continueReqSend {
		ctx.writer.Write(byteResponseContinue)
		ctx.writer.Flush()
		ctx.continueReqSend = true
		return nil
	}

	ctx.s.Handler(ctx)
	ctx.resp.Write(ctx.writer)
	if ctx.conn.Buffered() == 0 || ctx.req.ShouldClose() {
		err := ctx.writer.Flush()
		if err != nil {
			return errors.WithStack(err)
		}
	}
	if ctx.req.ShouldClose() || (ctx.s.MaxServeTimesPerConn > 0 && ctx.s.MaxServeTimesPerConn > ctx.connRequestNum) {
		return errors.New("should  close")
	}
	ctx.connRequestNum++
	return nil
}

var contextPool sync.Pool

func AcquireContext(s *Server, conn Conn) *Context {
	v := contextPool.Get()
	if v == nil {
		return NewContext(s, conn)
	}
	r := v.(*Context)
	r.Reset(conn)
	return r
}

func ReleaseContext(ctx *Context) {
	contextPool.Put(ctx)
}
