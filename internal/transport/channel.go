package transport

import (
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

// Record layer limits.
const (
	// MaxRecordPlaintext is the largest plaintext of one record. A larger
	// Write is split into several records.
	MaxRecordPlaintext = 16 << 10

	recordHeaderLen = 4
	recordTagLen    = chacha20poly1305.Overhead
	maxRecordCipher = MaxRecordPlaintext + recordTagLen
)

// Errors of the record layer. Any of them except ErrChannelClosed (a local
// Close) is fatal: the channel closes itself and the error is returned by
// every later Read and Write.
var (
	// ErrRecordAuth is returned for a record that does not authenticate: it
	// was modified, replayed, reordered or sealed under another key.
	ErrRecordAuth = errors.New("transport: record authentication failed")
	// ErrRecordSize is returned for a record whose length is out of range.
	ErrRecordSize = errors.New("transport: record length out of range")
	// ErrCounterExhausted is returned when a direction has used its last
	// nonce counter. The channel refuses to wrap the 64-bit counter: the
	// connection must be re-established, which derives fresh keys.
	ErrCounterExhausted = errors.New("transport: record counter exhausted")
	// ErrChannelClosed is returned after a local Close.
	ErrChannelClosed = fmt.Errorf("transport: channel closed: %w", net.ErrClosed)
)

// Channel is the AEAD record layer of AD-6: it carries a byte stream over a
// net.Conn as ChaCha20-Poly1305 records. It implements net.Conn so that yamux
// can run on it. Read and Write are safe for concurrent use, one reader and
// one writer at a time each.
//
// Wire format of a record: a uint32 big-endian length n of the ciphertext
// (plaintext plus 16-byte tag, at most MaxRecordPlaintext+16), followed by
// the n ciphertext bytes. Header and body are written with one Write call, so
// on a message transport such as a WebSocket every record is one message. The
// nonce is 4 zero bytes followed by a 64-bit big-endian record counter that is
// kept per direction and never sent; the 4-byte length header is the AEAD
// associated data. Because the counter is implicit, a replayed, reordered,
// dropped or modified record fails authentication and closes the channel.
type Channel struct {
	conn net.Conn

	wmu     sync.Mutex
	send    cipher.AEAD
	sendCtr uint64

	rmu     sync.Mutex
	recv    cipher.AEAD
	recvCtr uint64
	pending []byte // decrypted bytes not yet returned by Read

	emu sync.Mutex
	err error // first fatal error; set once
}

// newChannel builds the channel over conn with the given directional keys.
func newChannel(conn net.Conn, sendKey, recvKey []byte) (*Channel, error) {
	send, err := chacha20poly1305.New(sendKey)
	if err != nil {
		return nil, err
	}
	recv, err := chacha20poly1305.New(recvKey)
	if err != nil {
		return nil, err
	}
	return &Channel{conn: conn, send: send, recv: recv}, nil
}

func recordNonce(ctr uint64) [chacha20poly1305.NonceSize]byte {
	var n [chacha20poly1305.NonceSize]byte
	binary.BigEndian.PutUint64(n[4:], ctr)
	return n
}

// fatal records err as the channel's terminal error, closes the connection
// and returns the stored error.
func (c *Channel) fatal(err error) error {
	c.emu.Lock()
	if c.err == nil {
		c.err = err
	}
	err = c.err
	c.emu.Unlock()
	_ = c.conn.Close()
	return err
}

func (c *Channel) failure() error {
	c.emu.Lock()
	defer c.emu.Unlock()
	return c.err
}

// Write encrypts p into one or more records and writes them. It returns the
// number of plaintext bytes accepted.
func (c *Channel) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.failure(); err != nil {
		return 0, err
	}
	written := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > MaxRecordPlaintext {
			chunk = chunk[:MaxRecordPlaintext]
		}
		if c.sendCtr == math.MaxUint64 {
			return written, c.fatal(ErrCounterExhausted)
		}
		buf := make([]byte, recordHeaderLen, recordHeaderLen+len(chunk)+recordTagLen)
		binary.BigEndian.PutUint32(buf, uint32(len(chunk)+recordTagLen))
		nonce := recordNonce(c.sendCtr)
		buf = c.send.Seal(buf, nonce[:], chunk, buf[:recordHeaderLen])
		c.sendCtr++
		if _, err := c.conn.Write(buf); err != nil {
			return written, c.fatal(err)
		}
		written += len(chunk)
		p = p[len(chunk):]
	}
	return written, nil
}

// Read returns decrypted stream bytes. A clean end of the underlying
// connection between two records is io.EOF. A record that fails any check
// closes the channel.
func (c *Channel) Read(p []byte) (int, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	for {
		// The terminal error wins over buffered plaintext: after Close or a
		// fatal error no further data is handed to the consumer.
		if err := c.failure(); err != nil {
			c.pending = nil
			return 0, err
		}
		if len(c.pending) > 0 {
			break
		}
		if err := c.readRecord(); err != nil {
			return 0, err
		}
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

// readRecord reads, checks and decrypts one record into c.pending.
func (c *Channel) readRecord() error {
	var hdr [recordHeaderLen]byte
	if n, err := io.ReadFull(c.conn, hdr[:]); err != nil {
		if n == 0 {
			// Nothing of a record was consumed: a clean EOF or a timeout
			// leaves the channel usable.
			if lerr := c.failure(); lerr != nil {
				return lerr
			}
			return err
		}
		return c.fatal(fmt.Errorf("transport: truncated record header: %w", noEOF(err)))
	}
	size := binary.BigEndian.Uint32(hdr[:])
	if size <= recordTagLen || size > maxRecordCipher {
		return c.fatal(fmt.Errorf("%w: %d", ErrRecordSize, size))
	}
	if c.recvCtr == math.MaxUint64 {
		return c.fatal(ErrCounterExhausted)
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(c.conn, body); err != nil {
		return c.fatal(fmt.Errorf("transport: truncated record: %w", noEOF(err)))
	}
	nonce := recordNonce(c.recvCtr)
	plain, err := c.recv.Open(body[:0], nonce[:], body, hdr[:])
	if err != nil {
		return c.fatal(ErrRecordAuth)
	}
	c.recvCtr++
	c.pending = plain
	return nil
}

func noEOF(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

// Close closes the channel and the underlying connection.
func (c *Channel) Close() error {
	c.emu.Lock()
	if c.err == nil {
		c.err = ErrChannelClosed
	}
	c.emu.Unlock()
	return c.conn.Close()
}

// LocalAddr returns the local address of the underlying connection.
func (c *Channel) LocalAddr() net.Addr { return c.conn.LocalAddr() }

// RemoteAddr returns the remote address of the underlying connection.
func (c *Channel) RemoteAddr() net.Addr { return c.conn.RemoteAddr() }

// SetDeadline sets the read and write deadlines of the underlying connection.
// A read deadline that fires in the middle of a record closes the channel,
// because the record stream cannot be resynchronised.
func (c *Channel) SetDeadline(t time.Time) error { return c.conn.SetDeadline(t) }

// SetReadDeadline sets the read deadline of the underlying connection.
func (c *Channel) SetReadDeadline(t time.Time) error { return c.conn.SetReadDeadline(t) }

// SetWriteDeadline sets the write deadline of the underlying connection.
func (c *Channel) SetWriteDeadline(t time.Time) error { return c.conn.SetWriteDeadline(t) }

var _ net.Conn = (*Channel)(nil)
