package http1

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/url"
	"strconv"

	"github.com/pkg/errors"
	"github.com/valyala/bytebufferpool"
	"github.com/widaT/httparse"
)

const maxLineLength = 4096

var ErrLineTooLong = errors.New("header line too long")
var ErrBodyTooLarge = errors.New("body size  too large")

var defaultUserAgent = []byte("http1-client/1.1")

type RequestHeader struct {
	httparse.Request
	HTTP11 bool

	//Close means conn can't not reuse
	//not http1.1 close is true
	//connection value is close ,close is true
	//maybe some errors happend close need set true
	Close            bool
	ContentLength    int //-1 means chunked  -2 means identity
	Host             []byte
	TransferEncoding [][]byte
	URL              *url.URL
}

func (r *RequestHeader) Reset() {
	r.Request.Reset()
	r.HTTP11 = false
	r.Close = false
	r.Host = nil
	r.URL = nil
}

func (r *RequestHeader) Read(input []byte) (int, error) {
	n, err := r.Parse(input)
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (h *RequestHeader) Write(wr *bufio.Writer) error {
	host := h.Host
	uri := h.URL.RequestURI()
	if bytes.Equal(h.Method, byteConnect) && h.URL.Path == "" {
		uri = b2s(host)
		if h.URL.Opaque != "" {
			uri = h.URL.Opaque
		}
	}
	_, err := fmt.Fprintf(wr, "%s %s HTTP/1.1\r\n", h.Method, uri)
	if err != nil {
		return err
	}

	writeLine(wr, byteHost, host)

	userAgent := defaultUserAgent
	if len(h.GetHeader(HeaderUserAgent)) > 0 {
		userAgent = h.GetHeader(HeaderUserAgent)
	}
	if len(userAgent) > 0 {
		writeLine(wr, byteUserAgent, userAgent)
	}

	//@todo write Transfer

	if h.ContentLength > 0 {
		writeLine(wr, byteContentLength, s2b(strconv.Itoa(h.ContentLength)))
	}

	//write header
	for k, v := range h.Headers {
		writeLine(wr, s2b(k), v[0])
	}

	//end of header
	wr.Write(byteCRLF)
	return nil
}

var requestBodyPool bytebufferpool.Pool

type Request struct {
	RemoteAddr    string
	RequestHeader RequestHeader
	body          *bytebufferpool.ByteBuffer
	MaxBodySize   int
	userAgent     []byte

	bodyStream io.Reader
}

func (r *Request) Reset(RemoteAddr string) {
	r.RequestHeader.Reset()
	r.RemoteAddr = RemoteAddr
	r.MaxBodySize = 0
	//r.body not need to reset See `(r *Request) parse` method
	//r.body.Reset()

	if r.bodyStream != nil {
		if cl, ok := r.bodyStream.(io.Closer); ok {
			cl.Close()
		}
		r.bodyStream = nil
	}
}

func NewRequst(RemoteAddr string) *Request {
	return &Request{
		RemoteAddr: RemoteAddr,
		RequestHeader: RequestHeader{
			Request: *httparse.NewRequst(),
		},
	}
}

func (r *Request) Set(maxBodySize int) {
	r.MaxBodySize = maxLineLength
}

func (r *Request) ShouldClose() bool {
	return r.RequestHeader.Close
}

func (r *Request) needClose() {
	r.RequestHeader.Close = true
}

func (r *Request) Body() []byte {
	if r.body == nil {
		return nil
	}
	return r.body.B
}

func (r *Request) IsContinue() bool {
	if v := r.RequestHeader.GetHeader(HeaderExpect); bytes.Equal(v, byte100Continue) {
		return true
	}
	return false
}

func (r *Request) Parse(input []byte) (int, error) {
	n, err := r.parse(input)
	if err != nil {
		r.needClose()
	}
	return n, err
}

func (r *Request) ContinueReadBody(input []byte) (n int, err error) {
	if r.body == nil {
		r.body = requestBodyPool.Get()
	}
	bodyBuf := r.body
	bodyBuf.Reset()

	switch {
	case r.RequestHeader.ContentLength > 0:
		if r.MaxBodySize > 0 && r.RequestHeader.ContentLength > r.MaxBodySize {
			err = ErrBodyTooLarge
		}
		bodyBuf.B, err = appendBodyFixedSize(input, bodyBuf.B, r.RequestHeader.ContentLength)
		if err != nil {
			err = err
		}
		n = r.RequestHeader.ContentLength
	case r.RequestHeader.ContentLength == -1:
		bodyBuf.B, n, err = readChunked(input, r.MaxBodySize, bodyBuf.B)
		if err != nil {
			err = err
		}
	}
	return
}

func (r *Request) BodyRelease() {
	if r.body != nil {
		requestBodyPool.Put(r.body)
		r.body = nil
	}
}

func (r *Request) parse(input []byte) (int, error) {
	n, err := r.RequestHeader.Read(input)
	if err != nil {
		return 0, err
	}

	r.RequestHeader.ContentLength = -2

	r.RequestHeader.TransferEncoding, err = fixTransferEncoding(r.RequestHeader.Headers)
	if err != nil {
		return 0, err
	}

	realLength, err := fixLength(false, 0, r.RequestHeader.Method,
		r.RequestHeader.Headers, r.RequestHeader.TransferEncoding)
	if err != nil {
		return 0, err
	}

	r.RequestHeader.ContentLength = realLength

	//'Expect: 100-continue' header need to feedback a response to clinet ,no do it here
	if r.IsContinue() ||
		r.RequestHeader.ContentLength == 0 ||
		r.RequestHeader.ContentLength == -2 {
		return 0, nil
	}

	read, err := r.ContinueReadBody(input[:n])
	if err != nil {
		return 0, err
	}
	return n + read, err
}

//@todo
func (r *Request) Write(wr *bufio.Writer) error {
	if r.bodyStream != nil {
		return r.writeBodyStream(wr)
	}
	hasBody := r.body != nil && len(r.body.B) > 0

	if hasBody {
		r.RequestHeader.ContentLength = len(r.body.B)
	}

	if err := r.RequestHeader.Write(wr); err != nil {
		return errors.WithStack(err)
	}
	if hasBody {
		wr.Write(r.body.B)
	}
	if err := wr.Flush(); err != nil {
		return errors.WithStack(err)
	}
	return nil
}

func (r *Request) SetBodyStream(reader io.Reader, size int) {
	r.bodyStream = reader
	r.RequestHeader.ContentLength = size
}

func (r *Request) writeBodyStream(w *bufio.Writer) error {
	var err error
	contentLength := r.RequestHeader.ContentLength
	if contentLength < 0 {
		lr, ok := r.bodyStream.(*io.LimitedReader)
		if ok {
			if lr.N >= 0 {
				contentLength = int(lr.N)
				if int64(contentLength) != lr.N {
					contentLength = -1
				}
				if contentLength >= 0 {
					r.RequestHeader.ContentLength = contentLength
				}
			}
		}
	}
	if contentLength >= 0 {
		if err = r.RequestHeader.Write(w); err == nil {
			_, err = bufCopy(w, r.bodyStream)
			if err != nil {
				return errors.WithStack(err)
			}
		}
	} else {
		r.RequestHeader.ContentLength = -1
		if err = r.RequestHeader.Write(w); err == nil {
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

func readChunked(input []byte, maxBodySize int, dst []byte) ([]byte, int, error) {
	crlfLen := 2
	read := 0
	for {
		chunkSize, n, err := parseChunkSize(input)
		if err != nil {
			return dst, 0, err
		}
		read += n
		if maxBodySize > 0 && len(dst)+chunkSize > maxBodySize {
			return dst, 0, ErrBodyTooLarge
		}
		dst, err = appendBodyFixedSize(input[read:], dst, chunkSize+crlfLen)
		if err != nil {
			return dst, 0, err
		}
		if !bytes.Equal(dst[len(dst)-crlfLen:], byteCRLF) {
			return dst, 0, errors.Errorf("cannot find crlf at the end of chunk")
		}
		dst = dst[:len(dst)-crlfLen]
		if chunkSize == 0 {
			return dst, read, nil
		}
	}
}

func appendBodyFixedSize(input []byte, dst []byte, n int) ([]byte, error) {
	if n == 0 {
		return dst, nil
	}

	if len(input) < n {
		return nil, errors.New("Pending")
	}

	offset := len(dst)
	dstLen := offset + n
	if cap(dst) < dstLen {
		b := make([]byte, round2(dstLen))
		copy(b, dst)
		dst = b
	}
	dst = dst[:dstLen]

	copy(dst[offset:], input)
	return dst, nil
}

func round2(n int) int {
	if n <= 0 {
		return 0
	}
	n--
	x := uint(0)
	for n > 0 {
		n >>= 1
		x++
	}
	return 1 << x
}

func parseChunkSize(input []byte) (int, int, error) {
	var line []byte
	line, n, err := readChunkLine(input)
	if err != nil {
		return 0, 0, errors.WithStack(err)
	}
	len, err := parseHexUint(line)
	if err != nil {
		return 0, 0, errors.WithStack(err)
	}
	if len == 0 {
		return 0, 0, errors.WithStack(io.EOF)
	}
	return len, n, nil
}

func parseHexUint(v []byte) (n int, err error) {
	for i, b := range v {
		switch {
		case '0' <= b && b <= '9':
			b = b - '0'
		case 'a' <= b && b <= 'f':
			b = b - 'a' + 10
		case 'A' <= b && b <= 'F':
			b = b - 'A' + 10
		default:
			return 0, errors.New("invalid byte in chunk length")
		}
		if i == 16 {
			return 0, errors.New("http chunk length too large")
		}
		n <<= 4
		n |= int(b)
	}
	return
}

func readChunkLine(input []byte) ([]byte, int, error) {
	p := bytes.Index(input, []byte{'\n'})
	if p == -1 {
		return nil, 0, ErrLineTooLong
	}
	if p+1 >= maxLineLength {
		return nil, 0, ErrLineTooLong
	}
	b := make([]byte, p+1)
	copy(b, input[:p])
	b = trimTrailingWhitespace(b)
	b, err := removeChunkExtension(b)
	if err != nil {
		return nil, 0, err
	}
	return b, p + 1, nil
}

func trimTrailingWhitespace(b []byte) []byte {
	for len(b) > 0 && isASCIISpace(b[len(b)-1]) {
		b = b[:len(b)-1]
	}
	return b
}

func isASCIISpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

func removeChunkExtension(p []byte) ([]byte, error) {
	semi := bytes.IndexByte(p, ';')
	if semi == -1 {
		return p, nil
	}
	return p[:semi], nil
}
