package transport

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

// memConn is a net.Conn over an in-memory reader and writer.
type memConn struct {
	r      io.Reader
	w      io.Writer
	closed bool
}

func (m *memConn) Read(p []byte) (int, error)       { return m.r.Read(p) }
func (m *memConn) Write(p []byte) (int, error)      { return m.w.Write(p) }
func (m *memConn) Close() error                     { m.closed = true; return nil }
func (m *memConn) LocalAddr() net.Addr              { return pipeAddr{} }
func (m *memConn) RemoteAddr() net.Addr             { return pipeAddr{} }
func (m *memConn) SetDeadline(time.Time) error      { return nil }
func (m *memConn) SetReadDeadline(time.Time) error  { return nil }
func (m *memConn) SetWriteDeadline(time.Time) error { return nil }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "mem" }
func (pipeAddr) String() string  { return "mem" }

func testKey(b byte) []byte { return bytes.Repeat([]byte{b}, KeySize) }

// sealed returns the wire bytes a channel with the given send key writes for
// the given writes.
func sealed(t *testing.T, key []byte, writes ...[]byte) []byte {
	t.Helper()
	var out bytes.Buffer
	c, err := newChannel(&memConn{r: bytes.NewReader(nil), w: &out}, key, testKey(0))
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range writes {
		if _, err := c.Write(w); err != nil {
			t.Fatal(err)
		}
	}
	return out.Bytes()
}

// reader returns a channel that reads wire bytes sealed under key.
func reader(t *testing.T, key []byte, wire []byte) (*Channel, *memConn) {
	t.Helper()
	mc := &memConn{r: bytes.NewReader(wire), w: io.Discard}
	c, err := newChannel(mc, testKey(0), key)
	if err != nil {
		t.Fatal(err)
	}
	return c, mc
}

// splitRecords splits wire bytes into records.
func splitRecords(t *testing.T, wire []byte) [][]byte {
	t.Helper()
	var recs [][]byte
	for len(wire) > 0 {
		n := int(binary.BigEndian.Uint32(wire)) + recordHeaderLen
		recs = append(recs, wire[:n])
		wire = wire[n:]
	}
	return recs
}

func TestRecordVectors(t *testing.T) {
	var v vectorFile
	loadVectors(t, &v)
	if len(v.Records) == 0 {
		t.Fatal("no record vectors")
	}
	for _, r := range v.Records {
		key, _ := hex.DecodeString(r.Key)
		pt, _ := hex.DecodeString(r.Plaintext)
		wire, _ := hex.DecodeString(r.Wire)

		// Sealing with the counter preset reproduces the vector bytes.
		var out bytes.Buffer
		w, err := newChannel(&memConn{r: bytes.NewReader(nil), w: &out}, key, testKey(0))
		if err != nil {
			t.Fatal(err)
		}
		w.sendCtr = r.Counter
		if _, err := w.Write(pt); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(out.Bytes(), wire) {
			t.Errorf("counter %d: sealed %x, want %x", r.Counter, out.Bytes(), wire)
		}

		// Opening the vector bytes returns the plaintext.
		c, _ := reader(t, key, wire)
		c.recvCtr = r.Counter
		got := make([]byte, len(pt))
		if _, err := io.ReadFull(c, got); err != nil || !bytes.Equal(got, pt) {
			t.Errorf("counter %d: opened %x, %v; want %x", r.Counter, got, err, pt)
		}
	}
}

func TestChannelRoundTripOverPipe(t *testing.T) {
	a, b := net.Pipe()
	k1, k2 := testKey(1), testKey(2)
	ca, err := newChannel(a, k1, k2)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := newChannel(b, k2, k1)
	if err != nil {
		t.Fatal(err)
	}
	defer ca.Close()
	defer cb.Close()

	msg := make([]byte, 5*MaxRecordPlaintext+123)
	if _, err := rand.Read(msg); err != nil {
		t.Fatal(err)
	}
	reply := []byte("pong")
	errc := make(chan error, 1)
	go func() {
		n, err := ca.Write(msg)
		if err == nil && n != len(msg) {
			err = io.ErrShortWrite
		}
		errc <- err
	}()
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(cb, got); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatal("payload differs")
	}
	go func() { _, err := cb.Write(reply); errc <- err }()
	back := make([]byte, len(reply))
	if _, err := io.ReadFull(ca, back); err != nil || !bytes.Equal(back, reply) {
		t.Fatalf("reply %q, %v", back, err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if ca.LocalAddr() == nil || ca.RemoteAddr() == nil {
		t.Error("addresses not delegated")
	}
}

func TestChannelSmallReadsAndLargeWriteSplit(t *testing.T) {
	msg := bytes.Repeat([]byte("0123456789"), 4000) // 40000 bytes, three records
	wire := sealed(t, testKey(1), msg)
	if n := len(splitRecords(t, wire)); n != 3 {
		t.Fatalf("40000 bytes became %d records, want 3", n)
	}
	c, _ := reader(t, testKey(1), wire)
	var got []byte
	buf := make([]byte, 7)
	for len(got) < len(msg) {
		n, err := c.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, buf[:n]...)
	}
	if !bytes.Equal(got, msg) {
		t.Fatal("payload differs")
	}
}

func TestChannelEmptyWriteSendsNothing(t *testing.T) {
	if wire := sealed(t, testKey(1), nil, []byte{}); len(wire) != 0 {
		t.Fatalf("empty writes produced %d bytes", len(wire))
	}
}

func TestReplayedRecordClosesChannel(t *testing.T) {
	recs := splitRecords(t, sealed(t, testKey(1), []byte("one"), []byte("two")))
	wire := bytes.Join([][]byte{recs[0], recs[0], recs[1]}, nil) // record 0 twice
	c, mc := reader(t, testKey(1), wire)
	buf := make([]byte, 3)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "one" {
		t.Fatalf("first record: %q, %v", buf, err)
	}
	assertFatal(t, c, mc, ErrRecordAuth)
}

func TestReorderedRecordClosesChannel(t *testing.T) {
	recs := splitRecords(t, sealed(t, testKey(1), []byte("one"), []byte("two")))
	c, mc := reader(t, testKey(1), bytes.Join([][]byte{recs[1], recs[0]}, nil))
	assertFatal(t, c, mc, ErrRecordAuth)
}

func TestDroppedRecordClosesChannel(t *testing.T) {
	recs := splitRecords(t, sealed(t, testKey(1), []byte("one"), []byte("two")))
	c, mc := reader(t, testKey(1), recs[1])
	assertFatal(t, c, mc, ErrRecordAuth)
}

func TestModifiedRecordClosesChannel(t *testing.T) {
	good := sealed(t, testKey(1), []byte("a message"))
	for i := recordHeaderLen; i < len(good); i++ { // every ciphertext and tag byte
		bad := bytes.Clone(good)
		bad[i] ^= 0x01
		c, mc := reader(t, testKey(1), bad)
		assertFatal(t, c, mc, ErrRecordAuth)
	}
}

func TestModifiedLengthHeaderClosesChannel(t *testing.T) {
	good := sealed(t, testKey(1), []byte("a message"), bytes.Repeat([]byte("x"), 100))
	// Make the first record look one byte longer: it then swallows the first
	// byte of the next header and cannot authenticate, whatever follows.
	bad := bytes.Clone(good)
	binary.BigEndian.PutUint32(bad, binary.BigEndian.Uint32(bad)+1)
	c, mc := reader(t, testKey(1), bad)
	assertFatal(t, c, mc, ErrRecordAuth)
}

func TestWrongKeyAndReflectionClose(t *testing.T) {
	wire := sealed(t, testKey(1), []byte("secret"))
	c, mc := reader(t, testKey(9), wire)
	assertFatal(t, c, mc, ErrRecordAuth)

	// A record the channel sent itself, fed back to its own reader (the keys
	// of the two directions differ), does not authenticate.
	var out bytes.Buffer
	a, err := newChannel(&memConn{r: bytes.NewReader(nil), w: &out}, testKey(1), testKey(2))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	a.conn = &memConn{r: bytes.NewReader(out.Bytes()), w: io.Discard}
	if _, err := a.Read(make([]byte, 8)); !errors.Is(err, ErrRecordAuth) {
		t.Fatalf("reflected record: %v", err)
	}
}

func TestRecordSizeLimits(t *testing.T) {
	for _, n := range []uint32{0, recordTagLen, maxRecordCipher + 1, math.MaxUint32} {
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], n)
		c, mc := reader(t, testKey(1), append(hdr[:], make([]byte, 64)...))
		assertFatal(t, c, mc, ErrRecordSize)
	}
	// The largest legal record is accepted.
	c, _ := reader(t, testKey(1), sealed(t, testKey(1), make([]byte, MaxRecordPlaintext)))
	if n, err := io.ReadFull(c, make([]byte, MaxRecordPlaintext)); err != nil {
		t.Fatalf("max record: %d, %v", n, err)
	}
}

func TestTruncatedRecordClosesChannel(t *testing.T) {
	good := sealed(t, testKey(1), []byte("a message"))
	for _, cut := range []int{2, recordHeaderLen + 3, len(good) - 1} {
		c, mc := reader(t, testKey(1), good[:cut])
		if _, err := c.Read(make([]byte, 16)); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("cut %d: %v", cut, err)
		}
		if !mc.closed {
			t.Errorf("cut %d: connection not closed", cut)
		}
	}
}

func TestCleanEOFBetweenRecords(t *testing.T) {
	c, mc := reader(t, testKey(1), sealed(t, testKey(1), []byte("last")))
	if _, err := io.ReadFull(c, make([]byte, 4)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Read(make([]byte, 4)); err != io.EOF {
		t.Fatalf("got %v, want io.EOF", err)
	}
	if mc.closed {
		t.Error("a clean EOF must not close the connection by itself")
	}
}

func TestCounterWrapRefused(t *testing.T) {
	t.Run("send", func(t *testing.T) {
		var out bytes.Buffer
		c, _ := newChannel(&memConn{r: bytes.NewReader(nil), w: &out}, testKey(1), testKey(2))
		c.sendCtr = math.MaxUint64 - 1
		if _, err := c.Write([]byte("last record")); err != nil {
			t.Fatalf("last counter value must still work: %v", err)
		}
		n := out.Len()
		if _, err := c.Write([]byte("one too many")); !errors.Is(err, ErrCounterExhausted) {
			t.Fatalf("got %v, want ErrCounterExhausted", err)
		}
		if out.Len() != n {
			t.Error("a record was sent with a wrapped counter")
		}
		if _, err := c.Write([]byte("x")); !errors.Is(err, ErrCounterExhausted) {
			t.Fatalf("channel must stay refused: %v", err)
		}
	})
	t.Run("send within one Write", func(t *testing.T) {
		var out bytes.Buffer
		c, _ := newChannel(&memConn{r: bytes.NewReader(nil), w: &out}, testKey(1), testKey(2))
		c.sendCtr = math.MaxUint64 - 1
		n, err := c.Write(make([]byte, 2*MaxRecordPlaintext))
		if !errors.Is(err, ErrCounterExhausted) || n != MaxRecordPlaintext {
			t.Fatalf("got %d, %v", n, err)
		}
	})
	t.Run("receive", func(t *testing.T) {
		c, mc := reader(t, testKey(1), sealed(t, testKey(1), []byte("x")))
		c.recvCtr = math.MaxUint64
		assertFatal(t, c, mc, ErrCounterExhausted)
	})
}

func TestCloseIsFinal(t *testing.T) {
	a, b := net.Pipe()
	c, _ := newChannel(a, testKey(1), testKey(2))
	_ = b
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("x")); !errors.Is(err, ErrChannelClosed) || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("write after close: %v", err)
	}
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, ErrChannelClosed) {
		t.Fatalf("read after close: %v", err)
	}
}

func TestConcurrentWritesStayWhole(t *testing.T) {
	a, b := net.Pipe()
	k1, k2 := testKey(1), testKey(2)
	w, _ := newChannel(a, k1, k2)
	r, _ := newChannel(b, k2, k1)
	defer w.Close()
	defer r.Close()

	const writers, size = 8, 3*MaxRecordPlaintext + 5
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := w.Write(bytes.Repeat([]byte{byte('a' + i)}, size)); err != nil {
				t.Error(err)
			}
		}()
	}
	for i := 0; i < writers; i++ {
		msg := make([]byte, size)
		if _, err := io.ReadFull(r, msg); err != nil {
			t.Fatal(err)
		}
		if first := msg[0]; !bytes.Equal(msg, bytes.Repeat([]byte{first}, size)) {
			t.Fatalf("writes interleaved inside message %d", i)
		}
	}
	wg.Wait()
}

func assertFatal(t *testing.T, c *Channel, mc *memConn, want error) {
	t.Helper()
	n, err := c.Read(make([]byte, 1024))
	if !errors.Is(err, want) || n != 0 {
		t.Fatalf("Read = %d, %v; want %v", n, err, want)
	}
	if !mc.closed {
		t.Error("connection not closed after a fatal record error")
	}
	if _, err2 := c.Read(make([]byte, 1)); !errors.Is(err2, want) {
		t.Errorf("second Read = %v, want %v", err2, want)
	}
	if _, err2 := c.Write([]byte("x")); !errors.Is(err2, want) {
		t.Errorf("Write after failure = %v, want %v", err2, want)
	}
}

// FuzzRecordDecoder feeds arbitrary bytes to the record decoder. It must never
// panic, never return more than one record of plaintext per call, and never
// hand out data for bytes that were not sealed under the key (the seeds hold
// the only valid records, whose plaintext is checked).
func FuzzRecordDecoder(f *testing.F) {
	key := testKey(1)
	valid := func(msgs ...string) []byte {
		var out bytes.Buffer
		c, _ := newChannel(&memConn{r: bytes.NewReader(nil), w: &out}, key, testKey(0))
		for _, m := range msgs {
			_, _ = c.Write([]byte(m))
		}
		return out.Bytes()
	}
	f.Add(valid("hello"))
	f.Add(valid("a", "bb", "ccc"))
	f.Add([]byte{})
	f.Add([]byte{0, 0, 0, 17})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 1, 2, 3})
	f.Add(append(valid("x")[:8], 0xaa))
	f.Fuzz(func(t *testing.T, data []byte) {
		c, err := newChannel(&memConn{r: bytes.NewReader(data), w: io.Discard}, testKey(0), key)
		if err != nil {
			t.Fatal(err)
		}
		var total int
		buf := make([]byte, 1<<10)
		for i := 0; i < 64; i++ {
			n, err := c.Read(buf)
			total += n
			if n > MaxRecordPlaintext {
				t.Fatalf("returned %d bytes", n)
			}
			if err != nil {
				break
			}
		}
		if total > len(data) {
			t.Fatalf("decoded %d plaintext bytes from %d input bytes", total, len(data))
		}
	})
}

// vectors

type vectorFile struct {
	Transcript []struct {
		Name        string `json:"name"`
		WorkerHello string `json:"worker_hello"`
		HubHello    string `json:"hub_hello"`
		Hash        string `json:"hash"`
	} `json:"transcript"`
	KDF []struct {
		Name           string `json:"name"`
		EphWPriv       string `json:"eph_w_priv"`
		EphHPriv       string `json:"eph_h_priv"`
		EphWPub        string `json:"eph_w_pub"`
		EphHPub        string `json:"eph_h_pub"`
		DH             string `json:"dh"`
		ISK            string `json:"isk"`
		TranscriptHash string `json:"transcript_hash"`
		WorkerToHub    string `json:"worker_to_hub"`
		HubToWorker    string `json:"hub_to_worker"`
	} `json:"kdf"`
	Records []struct {
		Key       string `json:"key"`
		Counter   uint64 `json:"counter"`
		Plaintext string `json:"plaintext"`
		Wire      string `json:"wire"`
	} `json:"records"`
}

func loadVectors(t *testing.T, v *vectorFile) {
	t.Helper()
	b, err := os.ReadFile("testdata/handshake_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

// TestTerminalErrorBeatsBufferedPlaintext: once the channel has failed or was
// closed, Read hands out no more buffered plaintext.
func TestTerminalErrorBeatsBufferedPlaintext(t *testing.T) {
	key := testKey(5)
	wire := sealed(t, key, []byte("twelve bytes"))
	t.Run("close", func(t *testing.T) {
		c, _ := reader(t, key, wire)
		one := make([]byte, 1)
		if _, err := c.Read(one); err != nil {
			t.Fatal(err)
		}
		_ = c.Close()
		if n, err := c.Read(make([]byte, 16)); n != 0 || !errors.Is(err, ErrChannelClosed) {
			t.Fatalf("read after close with buffered data: %d, %v", n, err)
		}
	})
	t.Run("fatal write error", func(t *testing.T) {
		mc := &memConn{r: bytes.NewReader(wire), w: failWriter{}}
		c, err := newChannel(mc, testKey(0), key)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Read(make([]byte, 1)); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Write([]byte("x")); err == nil {
			t.Fatal("write on a failing connection succeeded")
		}
		if n, err := c.Read(make([]byte, 16)); n != 0 || err == nil {
			t.Fatalf("read after a fatal write error with buffered data: %d, %v", n, err)
		}
	})
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("boom") }
