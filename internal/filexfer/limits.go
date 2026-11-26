package filexfer

import (
	"errors"
	"io"
	"runtime"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// blockSize is the tar block size.
const blockSize = 512

// MaxEntryHeaderBytes is the budget of the header blocks of one archive entry
// on read: the ustar block, the pax header block and the pax records rounded
// up to blocks. proto.MaxEntryHeaderBytes (4 KiB, NFR-6) bounds the pax
// records; a path of proto.MaxPathBytes bytes alone needs a pax path record
// of 4108 bytes, so the records may take one block more than 4 KiB. The budget
// is 4 KiB plus three blocks (5632 bytes), which holds a maximal path and the
// mtime record and nothing else. The padding of the previous entry (at most
// 511 bytes) is counted separately.
const MaxEntryHeaderBytes = proto.MaxEntryHeaderBytes + 3*blockSize

// Limits are the effective limits of one transfer on this end. A zero member
// means the files.v1 default; a member above the default is lowered to it.
type Limits struct {
	// MaxBytes bounds the logical bytes (file content) of the transfer.
	MaxBytes int64
	// MaxEntries bounds the files and directories (the manifest not counted).
	MaxEntries int64
	// MaxDepth and MaxPathBytes bound each path, the destination prefix
	// included.
	MaxDepth     int
	MaxPathBytes int
	// MaxManifestBytes bounds the canonical manifest.
	MaxManifestBytes int64
	// ZstdMaxMemory bounds the zstd decoder; 0 is proto.ZstdMaxMemory, or
	// proto.ZstdMaxMemorySmall on a small device (SmallDevice).
	ZstdMaxMemory uint64
}

// DefaultLimits returns the files.v1 defaults.
func DefaultLimits() Limits {
	return Limits{}.Normalize()
}

// LimitsFrom returns the limits of an admission (ops.FileTransferLimits, the
// lower of the Hub's and the Worker's), normalized.
func LimitsFrom(l ops.FileTransferLimits) Limits {
	return Limits{MaxBytes: l.MaxBytes, MaxEntries: l.MaxEntries, MaxDepth: l.MaxDepth, MaxPathBytes: l.MaxPathBytes}.Normalize()
}

// Normalize replaces zero or out-of-range members by the defaults.
func (l Limits) Normalize() Limits {
	low64 := func(v, def int64) int64 {
		if v <= 0 || v > def {
			return def
		}
		return v
	}
	low := func(v, def int) int {
		if v <= 0 || v > def {
			return def
		}
		return v
	}
	l.MaxBytes = low64(l.MaxBytes, proto.MaxTransferBytes)
	l.MaxEntries = low64(l.MaxEntries, proto.MaxTransferEntries)
	l.MaxDepth = low(l.MaxDepth, proto.MaxPathDepth)
	l.MaxPathBytes = low(l.MaxPathBytes, proto.MaxPathBytes)
	l.MaxManifestBytes = low64(l.MaxManifestBytes, proto.MaxManifestBytes)
	mem := uint64(proto.ZstdMaxMemory)
	if SmallDevice() {
		mem = proto.ZstdMaxMemorySmall
	}
	if l.ZstdMaxMemory == 0 || l.ZstdMaxMemory > mem {
		l.ZstdMaxMemory = mem
	}
	return l
}

// MaxWire is the bound of encoded (wire) bytes of a transfer under l: the
// logical limit x 1.05 + 16 MiB (proto.MaxWireBytes) plus the archive
// overhead (headers and manifest), which a folder needs on top of content.
func (l Limits) MaxWire() int64 {
	return proto.MaxWireBytes(l.MaxDecoded())
}

// MaxDecoded is the bound of decoded archive bytes under l: the logical bytes,
// one header budget and one padding per entry, the manifest entry and the end
// of archive.
func (l Limits) MaxDecoded() int64 {
	perEntry := int64(MaxEntryHeaderBytes + blockSize)
	return l.MaxBytes + (l.MaxEntries+1)*perEntry + l.MaxManifestBytes + 2*blockSize
}

// checkPathLimits checks the depth and length of a full path against l.
func (l Limits) checkPathLimits(full string) error {
	if len(full) > l.MaxPathBytes {
		return tooLarge("a path is longer than the limit of this transfer")
	}
	depth := 1
	for i := 0; i < len(full); i++ {
		if full[i] == '/' {
			depth++
		}
	}
	if full != "" && depth > l.MaxDepth {
		return tooLarge("a path is deeper than the limit of this transfer")
	}
	return nil
}

// SmallDevice reports whether this build runs on a small device (Android or
// 32-bit ARM), which uses one encoder goroutine and the smaller zstd decoder
// memory bound (NFR-4, NFR-6).
func SmallDevice() bool {
	return runtime.GOOS == "android" || runtime.GOARCH == "arm"
}

func tooLarge(msg string) error  { return proto.NewError(proto.CodeTransferTooLarge, msg) }
func integrity(msg string) error { return proto.NewError(proto.CodeIntegrityFailed, msg) }
func conflict(msg string) error  { return proto.NewError(proto.CodeConflict, msg) }
func internal(msg string) error  { return proto.NewError(proto.CodeInternal, msg) }
func typeRefused(msg string) error {
	return proto.NewError(proto.CodeFileTypeRefused, msg)
}

// meter counts the bytes read from r and enforces a total bound and an
// optional per-phase budget. Once a bound is exceeded every Read returns the
// limit error.
type meter struct {
	r      io.Reader
	n      int64 // bytes read so far
	max    int64 // total bound
	budget int64 // bytes left in the current phase; < 0: no phase bound
	maxErr error // returned when max is exceeded
	budErr error // returned when budget is exceeded
	err    error // sticky limit error
	srcErr error // the last error of r other than io.EOF
}

func newMeter(r io.Reader, max int64, maxErr error) *meter {
	return &meter{r: r, max: max, budget: -1, maxErr: maxErr}
}

func (m *meter) setBudget(b int64, err error) { m.budget, m.budErr = b, err }

func (m *meter) Read(p []byte) (int, error) {
	if m.err != nil {
		return 0, m.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	lim, limErr := m.max-m.n, m.maxErr
	if m.budget >= 0 && m.budget < lim {
		lim, limErr = m.budget, m.budErr
	}
	// Read one byte more than allowed, so that a stream that ends exactly at
	// the bound is not refused and one that goes on is.
	if int64(len(p)) > lim+1 {
		p = p[:lim+1]
	}
	n, err := m.r.Read(p)
	if int64(n) > lim {
		m.err = limErr
		return 0, m.err
	}
	m.n += int64(n)
	if m.budget >= 0 {
		m.budget -= int64(n)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		m.srcErr = err
	}
	return n, err
}
