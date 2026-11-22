package presence

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
)

// Status is what a reader concludes about one record.
type Status string

const (
	// StatusUnavailable: the record could not be read or did not validate
	// (missing, wrong owner or mode, not a regular file, too large, bad
	// schema, a timestamp in the future).
	StatusUnavailable Status = "unavailable"
	// StatusStopped: the daemon published a stopped marker.
	StatusStopped Status = "stopped"
	// StatusStale: the record says running but it was last updated at least
	// stale_after_s ago (a crash, or a heartbeat that does not arrive).
	StatusStale Status = "stale"
	// StatusRunning: running and updated less than stale_after_s ago.
	StatusRunning Status = "running"
)

// Source says which kind of directory a candidate came from.
type Source string

const (
	SourcePrivate Source = "private"
	SourcePublic  Source = "public"
)

// Candidate is one presence file found by enumeration, not yet trusted.
type Candidate struct {
	Source   Source
	Dir      string
	Instance string
}

// Result is the verified answer for one candidate.
type Result struct {
	Candidate Candidate
	Status    Status
	Record    *Record // nil when unavailable
	Age       time.Duration
	Err       error // why unavailable
}

// Reader is the secure consumer of presence records. The zero value is not
// useful; build one with [NewReader] or fill the fields (tests do).
type Reader struct {
	// UID is the reader's uid: owner of a valid private record.
	UID int
	// RootUID owns public records; 0 in production.
	RootUID int
	// PrivateDirs are private presence directories to look in.
	PrivateDirs []string
	// DiscoveryDirs hold pointers to relocated private directories.
	DiscoveryDirs []string
	// PublicRoot is the public presence directory; empty: none.
	PublicRoot string
	// Now defaults to time.Now.
	Now func() time.Time
}

// NewReader returns the reader of the current account: the default private
// directory, the default discovery entries and the public directory of this
// OS. stateDirs adds explicit state directories (the selected profile, an
// environment override).
func NewReader(stateDirs ...string) Reader {
	r := Reader{UID: os.Geteuid(), PublicRoot: PublicRootFor(runtime.GOOS)}
	env := config.SystemPathEnv()
	add := func(def string) {
		r.PrivateDirs = append(r.PrivateDirs, filepath.Join(def, privateDirName))
		r.DiscoveryDirs = append(r.DiscoveryDirs, filepath.Join(def, discoveryDirName))
	}
	if def, err := env.StateDir(); err == nil {
		add(def)
	}
	if runtime.GOOS == "linux" {
		// A service started without XDG_STATE_HOME lives under the home
		// directory; a login shell may set it.
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			add(filepath.Join(home, ".local", "state", "minion"))
		}
	}
	if d := os.Getenv("MINION_STATE_DIR"); filepath.IsAbs(d) {
		stateDirs = append(stateDirs, d)
	}
	for _, d := range stateDirs {
		if d != "" {
			r.PrivateDirs = append(r.PrivateDirs, filepath.Join(d, privateDirName))
		}
	}
	return r
}

func (r Reader) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Scan enumerates private and public candidates independently and verifies
// each. A directory that does not exist is skipped silently; a directory that
// exists but is not trustworthy yields one unavailable result with an empty
// instance. Results are ordered running, stale, stopped, unavailable, newest
// first.
func (r Reader) Scan() []Result {
	var out []Result
	seen := map[string]bool{}
	scanDir := func(src Source, dir string) {
		phys, err := r.checkDir(src, dir)
		if err != nil {
			if !isNotExist(err) {
				out = append(out, Result{Candidate: Candidate{Source: src, Dir: dir}, Status: StatusUnavailable, Err: err})
			}
			return
		}
		if seen[phys] {
			return
		}
		seen[phys] = true
		for _, c := range r.list(src, phys) {
			out = append(out, r.Check(c))
		}
	}
	dirs := append([]string(nil), r.PrivateDirs...)
	for _, d := range r.DiscoveryDirs {
		dirs = append(dirs, r.followIndex(d)...)
	}
	for _, d := range dirs {
		scanDir(SourcePrivate, d)
	}
	if r.PublicRoot != "" {
		scanDir(SourcePublic, r.PublicRoot)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if rank(a.Status) != rank(b.Status) {
			return rank(a.Status) < rank(b.Status)
		}
		return updated(a) > updated(b)
	})
	return out
}

func rank(s Status) int {
	switch s {
	case StatusRunning:
		return 0
	case StatusStale:
		return 1
	case StatusStopped:
		return 2
	}
	return 3
}

func updated(r Result) int64 {
	if r.Record == nil {
		return 0
	}
	return r.Record.UpdatedAt
}

// Best returns the first result of [Reader.Scan] order, or an unavailable
// result when there is none.
func Best(results []Result) Result {
	if len(results) == 0 {
		return Result{Status: StatusUnavailable, Err: os.ErrNotExist}
	}
	return results[0]
}

func (r Reader) checkDir(src Source, dir string) (string, error) {
	if src == SourcePublic {
		return verifyPublicDir(dir, r.RootUID)
	}
	return verifyPrivateDir(dir, r.UID)
}

func (r Reader) followIndex(dir string) []string {
	phys, err := verifyPrivateDir(dir, r.UID)
	if err != nil {
		return nil
	}
	ents, err := os.ReadDir(phys)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if filepath.Ext(e.Name()) != recordSuffix {
			continue
		}
		if d, err := readIndex(phys, e.Name(), r.UID); err == nil {
			out = append(out, d)
		}
	}
	return out
}

func (r Reader) list(src Source, dir string) []Candidate {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Candidate
	for _, e := range ents {
		if id, usage, ok := splitName(e.Name()); ok && !usage {
			out = append(out, Candidate{Source: src, Dir: dir, Instance: id})
		}
	}
	return out
}

// Check opens the candidate's record once, validates the handle and the
// content, and classifies it.
func (r Reader) Check(c Candidate) Result {
	res := Result{Candidate: c, Status: StatusUnavailable}
	if !ValidInstance(c.Instance) {
		res.Err = os.ErrInvalid
		return res
	}
	phys, err := r.checkDir(c.Source, c.Dir)
	if err != nil {
		res.Err = err
		return res
	}
	raw, err := r.readFile(c.Source, phys, c.Instance+recordSuffix, MaxRecordSize)
	if err != nil {
		res.Err = err
		return res
	}
	rec, err := DecodeRecord(raw)
	if err != nil {
		res.Err = err
		return res
	}
	if rec.Instance != c.Instance {
		res.Err = os.ErrInvalid
		return res
	}
	wantScope := ScopeUser
	if c.Source == SourcePublic {
		wantScope = ScopeRoot
	}
	if rec.OSScope != wantScope {
		res.Err = os.ErrInvalid
		return res
	}
	age := r.now().Sub(time.UnixMilli(rec.UpdatedAt))
	if age < 0 {
		res.Err = errFuture
		return res
	}
	res.Record, res.Age = &rec, age
	switch {
	case rec.State == StateStopped:
		res.Status = StatusStopped
	case age < time.Duration(rec.StaleAfterS)*time.Second:
		res.Status = StatusRunning
	default:
		res.Status = StatusStale
	}
	return res
}

// ReadUsage returns the usage-meters document stored beside the candidate's
// record, after the same handle checks, bounded to [MaxUsageSize]. The envelope
// (a JSON object with schema minion.usage-meters/1) is checked; the content is
// the verified document of the Hub.
func (r Reader) ReadUsage(c Candidate) ([]byte, error) {
	if !ValidInstance(c.Instance) {
		return nil, os.ErrInvalid
	}
	phys, err := r.checkDir(c.Source, c.Dir)
	if err != nil {
		return nil, err
	}
	raw, err := r.readFile(c.Source, phys, c.Instance+usageSuffix, MaxUsageSize)
	if err != nil {
		return nil, err
	}
	if err := checkUsage(raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func (r Reader) readFile(src Source, dir, name string, max int64) ([]byte, error) {
	if src == SourcePublic {
		return readPublicFile(dir, name, r.RootUID, max)
	}
	return readPrivateFile(dir, name, r.UID, max)
}
