package proxy

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"strings"
)

// prefixConn lets a buffered reader that has already consumed bytes from a
// connection (the CONNECT line) hand the rest to a TLS server that reads from
// the raw conn.
type prefixConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *prefixConn) Read(b []byte) (int, error) {
	if c.r != nil {
		if n := c.r.Buffered(); n > 0 {
			return c.r.Read(b)
		}
		c.r = nil
	}
	return c.Conn.Read(b)
}

// readBounded reads up to max+1 bytes; the extra byte tells us the stream was
// larger than the cap without buffering all of it.
func readBounded(r io.Reader, max int) (data []byte, tooLarge bool) {
	if r == nil {
		return nil, false
	}
	buf, err := io.ReadAll(io.LimitReader(r, int64(max)+1))
	if err != nil {
		return buf, false
	}
	if len(buf) > max {
		return buf[:max], true
	}
	return buf, false
}

func newBytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// httpTokenContains reports whether a comma-separated header value contains a
// token, case-insensitively.
func httpTokenContains(header, token string) bool {
	for _, part := range strings.Split(header, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}
