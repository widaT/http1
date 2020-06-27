package http1

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/pkg/errors"
	"github.com/valyala/bytebufferpool"
	"github.com/widaT/httparse"
)

var (
	defalutServer = []byte("http1-server")
)

type ResponseHeader struct {
	httparse.Response
	ContentLength    int
	TransferEncoding [][]byte
	Close            bool
	HTTP11           bool
	Server           []byte
	ContentType      []byte
}

func NewResponseHeader() *ResponseHeader {
	return &ResponseHeader{
		Response: httparse.Response{
			Proto:      byteHTTP11,
			StatusCode: 200,
		},
		HTTP11:      true,
		Server:      defalutServer,
		ContentType: defaultContentType,
	}
}

func (h *ResponseHeader) Reset() {
	h.Response.Reset()
	h.ContentLength = 0
	h.Close = false
	h.HTTP11 = false
	//h.Server = nil
	h.ContentType = nil
}

func (h *ResponseHeader) Write(w *bufio.Writer) error {
	if h.StatusCode <= 0 {
		h.StatusCode = StatusOK
	}
	w.Write(responeFirstLine(h.StatusCode))

	if len(h.Server) != 0 {
		writeLine(w, byteServer, h.Server)
	}

	writeLine(w, byteDate, s2b(time.Now().Format(time.RFC850)))
	if h.ContentLength != 0 || len(h.ContentType) > 0 {
		if len(h.ContentType) > 0 {
			writeLine(w, byteContentType, h.ContentType)
		}
	}
	if h.ContentLength > 0 && !h.mustIgnoreContentLength() {
		l := strconv.Itoa(h.ContentLength)
		writeLine(w, byteContentLength, s2b(l))
	}

	if h.ContentLength == -1 && chunked(h.TransferEncoding) {
		writeLine(w, byteTransferEncoding, byteChunked)
	}

	if h.ContentLength == -2 {
		h.Close = true
		writeLine(w, byteTransferEncoding, byteIdentity)

	}

	for k, v := range h.Response.Headers {
		writeLine(w, s2b(k), v[0])
	}

	if h.Close {
		writeLine(w, byteConnection, byteClose)
	}

	//end of header
	w.Write(byteCRLF)
	return nil
}

func (h *ResponseHeader) SetContentLength(n int) {
	if h.mustIgnoreContentLength() {
		h.ContentLength = 0
		return
	}
	if chunked(h.TransferEncoding) {
		h.ContentLength = -1
		return
	}
	h.ContentLength = n
	delete(h.Response.Headers, HeaderTransferEncoding)
}

var responseBytePool bytebufferpool.Pool

type Response struct {
	header     ResponseHeader
	body       *bytebufferpool.ByteBuffer
	bodyStream io.Reader
	noBody     bool
}

func NewResponse() *Response {
	return &Response{
		header: *NewResponseHeader(),
	}
}

func (r *Response) Reset() {
	r.header.Reset()

	//body is not need to reset see  (r *Response) ReadBody method
	//if r.Body != nil {
	//r.Body.Reset()
	//responseBytePool.Put(r.Body)
	//}
	r.noBody = false
	if r.bodyStream != nil {
		if cl, ok := r.bodyStream.(io.Closer); ok {
			cl.Close()
		}
		r.bodyStream = nil
	}
}

func (r *Response) SetStatusCode(statusCode int) {
	r.header.Response.StatusCode = statusCode
}

func (r *Response) SetContentType(contentType []byte) {
	r.header.ContentType = contentType
}

func (r *Response) SetClose(b bool) {
	r.header.Close = b
}

func (r *Response) bodyBuffer() *bytebufferpool.ByteBuffer {
	if r.body == nil {
		r.body = responseBytePool.Get()
	}
	return r.body
}

func (r *Response) SetBody(body []byte) {
	bodyBuf := r.bodyBuffer()
	bodyBuf.Reset()
	bodyBuf.Write(body)
}

func (r *Response) Body() []byte {
	if r.body == nil {
		return nil
	}
	return r.body.B
}

func (r *Response) BodyRelease() {
	if r.body != nil {
		requestBodyPool.Put(r.body)
		r.body = nil
	}
}

func (r *Response) SendFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	fileInfo, err := f.Stat()

	if err != nil {
		f.Close()
		return err
	}
	size64 := fileInfo.Size()
	size := int(size64)
	if int64(size) != size64 {
		size = -1
	}
	r.SetBodyStream(f, size)
	return nil
}

func (r *Response) SetBodyStream(reader io.Reader, size int) {
	r.bodyStream = reader
	r.header.ContentLength = size
}

func (r *Response) Write(w *bufio.Writer) error {
	if r.bodyStream != nil {
		return r.writeBodyStream(w)
	}

	hasBody := !r.noBody
	var body []byte
	if r.body != nil {
		body = r.body.B
		bodyLen := len(body)
		if hasBody || bodyLen > 0 {
			r.header.SetContentLength(bodyLen)
		}
	}
	if err := r.header.Write(w); err != nil {
		return err
	}
	if hasBody {
		//defer r.Body.Reset()
		if _, err := w.Write(body); err != nil {
			return err
		}
	}
	return nil
}

func (r *Response) writeBodyStream(w *bufio.Writer) error {
	var err error
	contentLength := r.header.ContentLength
	if contentLength < 0 {
		lr, ok := r.bodyStream.(*io.LimitedReader)
		if ok {
			if lr.N >= 0 {
				contentLength = int(lr.N)
				if int64(contentLength) != lr.N {
					contentLength = -1
				}
				if contentLength >= 0 {
					r.header.ContentLength = contentLength
				}
			}
		}
	}
	if contentLength >= 0 {
		if err = r.header.Write(w); err == nil {
			_, err = bufCopy(w, r.bodyStream)
			if err != nil {
				return errors.WithStack(err)
			}
		}
	} else {
		r.header.ContentLength = -1
		if err = r.header.Write(w); err == nil {
			err = writeChunked(w, r.bodyStream)
		}
	}
	if r.bodyStream == nil {
		return nil
	}
	if cl, ok := r.bodyStream.(io.Closer); ok {
		err = cl.Close()
	}
	r.bodyStream = nil
	return err
}

var responseBodyPool bytebufferpool.Pool

func (h *ResponseHeader) mustIgnoreContentLength() bool {
	if h.StatusCode < 100 || h.StatusCode == StatusOK {
		return false
	}
	return h.StatusCode == StatusNotModified || h.StatusCode == StatusNoContent || h.StatusCode < 200
}

func responeFirstLine(statusCode int) []byte {
	b := cache[statusCode]
	if len(b) > 0 {
		return b
	}
	return s2b(fmt.Sprintf("HTTP/1.1 %d %s\r\n", statusCode, reason(statusCode)))
}
