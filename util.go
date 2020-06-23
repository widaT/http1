package http1

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strconv"
	"sync"
	"unsafe"

	"github.com/pingcap/errors"
	"github.com/widaT/httparse"
)

func b2s(b []byte) string {
	return *(*string)(unsafe.Pointer(&b))
}

func s2b(s string) (b []byte) {
	x := (*[2]uintptr)(unsafe.Pointer(&s))
	h := [3]uintptr{x[0], x[1], x[1]}
	return *(*[]byte)(unsafe.Pointer(&h))
}

func appendLine(dst, key, value []byte) []byte {
	dst = append(dst, key...)
	dst = append(dst, byteColonSpace...)
	dst = append(dst, value...)
	return append(dst, byteCRLF...)
}

func writeLine(wr *bufio.Writer, key, value []byte) {
	wr.Write(key)
	wr.Write(byteColonSpace)
	wr.Write(value)
	wr.Write(byteCRLF)
}

func fixTransferEncoding(header httparse.Header) ([][]byte, error) {
	raw, present := header[HeaderTransferEncoding]
	if !present {
		return nil, nil
	}
	header.Del(HeaderTransferEncoding)
	encodings := bytes.Split(raw[0], []byte(","))
	tr := make([][]byte, 0, len(encodings))
	for _, encoding := range encodings {
		encoding = bytes.TrimSpace(encoding)
		// "identity" encoding is not recorded
		if bytes.Equal(encoding, byteIdentity) {
			break
		}
		if !bytes.Equal(encoding, byteChunked) {
			return nil, errors.Errorf(fmt.Sprintf("unsupported transfer encoding: %q", encoding))
		}
		tr = tr[0 : len(tr)+1]
		tr[len(tr)-1] = encoding
	}
	if len(tr) > 1 {
		return nil, errors.Errorf("too many transfer encodings")
	}
	if len(tr) > 0 {
		header.Del(HeaderTransferEncoding)
		return tr, nil
	}

	return nil, nil
}

func fixLength(isResponse bool, status int, requestMethod []byte,
	header httparse.Header, TransferEncoding [][]byte) (int, error) {
	isRequest := !isResponse
	contentLens := header[HeaderContentLength]

	if len(contentLens) > 1 {
		first := bytes.TrimSpace(contentLens[0])
		for _, ct := range contentLens[1:] {
			if bytes.Equal(first, bytes.TrimSpace(ct)) {
				return 0, fmt.Errorf("http: message cannot contain multiple Content-Length headers; got %q", contentLens)
			}
		}
		header.Del(HeaderContentLength)
		header.Add(HeaderContentLength, first)
		contentLens = header[HeaderContentLength]
	}

	if !isResponse && bytes.Equal(requestMethod, byteHead) {
		if isRequest && len(contentLens) > 0 && !(len(contentLens) == 1 && bytes.Equal(contentLens[0], []byte("0"))) {
			return 0, fmt.Errorf("http: method cannot contain a Content-Length; got %q", contentLens)
		}
		return 0, nil
	}
	if status/100 == 1 {
		return 0, nil
	}
	switch status {
	case 204, 304:
		return 0, nil
	}

	if chunked(TransferEncoding) {
		return -1, nil
	}

	var cl []byte
	if len(contentLens) == 1 {
		cl = bytes.TrimSpace(contentLens[0])
	}
	if len(cl) > 0 {
		n, err := parseContentLength(cl)
		if err != nil {
			return -2, err
		}
		return n, nil
	}
	header.Del(HeaderContentLength)
	if isRequest {
		return 0, nil
	}
	return -2, nil
}

func parseContentLength(cl []byte) (int, error) {
	cl = bytes.TrimSpace(cl)
	if len(cl) == 0 {
		return -1, nil
	}
	n, err := strconv.ParseInt(b2s(cl), 10, 64)
	if err != nil || n < 0 {
		return 0, errors.Errorf("bad Content-Length %s", cl)
	}
	return int(n), nil

}
func chunked(te [][]byte) bool {
	return len(te) > 0 && bytes.Equal(te[0], byteChunked)
}

var bufPool = sync.Pool{
	New: func() interface{} {
		return make([]byte, 4096)
	},
}

func bufCopy(w io.Writer, r io.Reader) (int64, error) {
	b := bufPool.Get().([]byte)
	n, err := io.CopyBuffer(w, r, b)
	bufPool.Put(b)
	return n, err
}

func writeChunked(w *bufio.Writer, r io.Reader) error {
	buf := bufPool.Get().([]byte)

	var err error
	var n int
	for {
		n, err = r.Read(buf)
		if n == 0 {
			if err == io.EOF {
				if err = writeChunkBlock(w, buf[:0]); err != nil {
					break
				}
				err = nil
			}
			break
		}
		if err = writeChunkBlock(w, buf[:n]); err != nil {
			break
		}
	}
	bufPool.Put(buf)
	return err
}

func writeChunkBlock(w *bufio.Writer, b []byte) error {
	_, err := fmt.Fprintf(w, "%x\r\n", len(b))
	if err != nil {
		return errors.WithStack(err)
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	_, err = w.Write(byteCRLF)
	err1 := w.Flush()
	if err == nil {
		err = err1
	}
	return err
}
