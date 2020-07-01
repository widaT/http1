package http1

import (
	"bytes"
	"io"
	"net/url"

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

var requestBodyPool bytebufferpool.Pool

type Request struct {
	header              RequestHeader
	body                *bytebufferpool.ByteBuffer
	MaxBodySize         int
	parseHeaderComplete bool
}

func (r *Request) Reset() {
	r.header.Reset()
	r.MaxBodySize = 0
	r.parseHeaderComplete = false
	//r.body not need to reset See `(r *Request) parse` method
	//r.body.Reset()
}

func NewRequst(RemoteAddr string) *Request {
	return &Request{
		header: RequestHeader{
			Request: *httparse.NewRequst(),
		},
	}
}

func (r *Request) Set(maxBodySize int) {
	r.MaxBodySize = maxLineLength
}

func (r *Request) ShouldClose() bool {
	return r.header.Close
}

func (r *Request) needClose() {
	r.header.Close = true
}

func (r *Request) Body() []byte {
	if r.body == nil {
		return nil
	}
	return r.body.B
}

func (r *Request) Header() *RequestHeader {
	return &r.header
}

func (r *Request) IsContinue() bool {
	if v := r.header.GetHeader(HeaderExpect); bytes.Equal(v, byte100Continue) {
		return true
	}
	return false
}

func (r *Request) Parse(input Conn) error {
	err := r.parse(input)
	if err != nil {
		if err != StatusPartial {
			r.needClose()
		}
	}
	return err
}

func (r *Request) ContinueReadBody(input Conn) (err error) {
	if r.body == nil {
		r.body = requestBodyPool.Get()
	}
	bodyBuf := r.body
	bodyBuf.Reset()

	switch {
	case r.header.ContentLength > 0:
		if r.MaxBodySize > 0 && r.header.ContentLength > r.MaxBodySize {
			err = ErrBodyTooLarge
			return
		}
		if input.Buffered() < r.header.ContentLength {
			err = StatusPartial
			return
		}

		buf, err0 := input.Bytes()
		if err0 != nil {
			return err0
		}

		bodyBuf.B, err = appendBodyFixedSize(buf, bodyBuf.B, r.header.ContentLength)
		if err != nil {
			return
		}
	case r.header.ContentLength == -1:
		bodyBuf.B, err = readChunked(input, r.MaxBodySize, bodyBuf.B)
		if err != nil {
			return
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

func (r *Request) parse(input Conn) (err error) {
	if !r.parseHeaderComplete {

		buf, err0 := input.Bytes()
		if err0 != nil {
			return err0
		}
		n, err := r.header.Read(buf)
		if err != nil {
			return err
		}
		input.Shift(n)
		r.parseHeaderComplete = true
	}

	r.header.ContentLength = -2

	r.header.TransferEncoding, err = fixTransferEncoding(r.header.Headers)
	if err != nil {
		return err
	}

	realLength, err := fixLength(false, 0, r.header.Method,
		r.header.Headers, r.header.TransferEncoding)
	if err != nil {
		return err
	}

	r.header.ContentLength = realLength

	//'Expect: 100-continue' header need to feedback a response to clinet ,no do it here
	if r.IsContinue() ||
		r.header.ContentLength == 0 ||
		r.header.ContentLength == -2 {
		return nil
	}

	err = r.ContinueReadBody(input)
	if err != nil {
		return err
	}
	return
}

func readChunked(input Conn, maxBodySize int, dst []byte) ([]byte, error) {
	crlfLen := 2
	//read := 0
	for {
		buf, err := input.Bytes()
		if err != nil {
			return nil, err
		}
		chunkSize, n, err := parseChunkSize(buf)
		if err != nil {
			return dst, err
		}
		input.Shift(n)
		if maxBodySize > 0 && len(dst)+chunkSize > maxBodySize {
			return dst, ErrBodyTooLarge
		}
		buf, _ = input.Bytes()
		dst, err = appendBodyFixedSize(buf, dst, chunkSize+crlfLen)
		if err != nil {
			return dst, err
		}
		if !bytes.Equal(dst[len(dst)-crlfLen:], byteCRLF) {
			return dst, errors.Errorf("cannot find crlf at the end of chunk")
		}
		dst = dst[:len(dst)-crlfLen]
		if chunkSize == 0 {
			return dst, nil
		}
	}
}

func appendBodyFixedSize(input []byte, dst []byte, n int) ([]byte, error) {
	if n == 0 {
		return dst, nil
	}

	if len(input) < n {
		return nil, StatusPartial
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
		return 0, 0, err
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
		return nil, 0, StatusPartial
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
