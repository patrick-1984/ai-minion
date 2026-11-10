package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/patrick-1984/ai-minion/internal/identity"
)

// Names inside <data>/tls/ for the generated self-signed certificate.
const (
	// TLSDir is the directory inside the data dir that holds the certificate.
	TLSDir = "tls"
	// TLSCertFile and TLSKeyFile are the PEM files of the generated
	// certificate and its ECDSA P-256 key.
	TLSCertFile = "cert.pem"
	TLSKeyFile  = "key.pem"
)

const (
	// selfSignedValidity is below the 825 days that Apple platforms accept
	// for a manually trusted certificate.
	selfSignedValidity = 800 * 24 * time.Hour
	// renewBefore is how long before expiry a start replaces a generated
	// certificate.
	renewBefore = 30 * 24 * time.Hour
)

// CertSource holds the Hub's TLS certificate and serves it to the TLS
// listener. [CertSource.Reload] swaps in a new certificate from the same
// files without dropping connections (SIGHUP). It is safe for concurrent use.
type CertSource struct {
	certFile, keyFile string
	cur               atomic.Pointer[tls.Certificate]
	// Generated is true when this start created the self-signed certificate.
	Generated bool
	// SelfSigned is true when the certificate is the Hub's own generated one,
	// false when the operator supplied it.
	SelfSigned bool
}

// CertOptions says where the certificate comes from.
type CertOptions struct {
	// DataDir holds <data>/tls/ for the generated certificate.
	DataDir string
	// CertFile and KeyFile name an operator-supplied certificate. Both or
	// neither.
	CertFile, KeyFile string
	// Names are extra host names or IPs the generated certificate should
	// cover (the public URL host, the listen host).
	Names []string
	// Now is the clock; nil means [time.Now].
	Now func() time.Time
}

// LoadCertSource returns the certificate source: the operator's files when
// given, otherwise the generated self-signed certificate in <data>/tls/,
// created on first start and reused later.
func LoadCertSource(o CertOptions) (*CertSource, error) {
	if (o.CertFile == "") != (o.KeyFile == "") {
		return nil, errors.New("set both MINION_TLS_CERT_FILE and MINION_TLS_KEY_FILE, or neither")
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	cs := &CertSource{}
	if o.CertFile != "" {
		cs.certFile, cs.keyFile = o.CertFile, o.KeyFile
		if err := cs.Reload(); err != nil {
			return nil, err
		}
		return cs, nil
	}
	if o.DataDir == "" {
		return nil, errors.New("tls: data directory is empty")
	}
	cs.SelfSigned = true
	dir := filepath.Join(o.DataDir, TLSDir)
	cs.certFile, cs.keyFile = filepath.Join(dir, TLSCertFile), filepath.Join(dir, TLSKeyFile)

	usable, err := existingUsable(cs.certFile, cs.keyFile, o.Names, now())
	if err != nil {
		return nil, err
	}
	if !usable {
		cs.Generated, err = generateLocked(dir, cs.certFile, cs.keyFile, o.Names, now)
		if err != nil {
			return nil, err
		}
	}
	if err := cs.Reload(); err != nil {
		return nil, err
	}
	return cs, nil
}

// existingUsable reports whether the generated files exist and can be kept.
// Files that exist but cannot be read as a certificate and key are an error:
// they are never overwritten silently. A certificate that expires soon, or
// does not cover a configured name, is replaced.
func existingUsable(certPath, keyPath string, names []string, now time.Time) (bool, error) {
	_, certErr := os.Stat(certPath)
	_, keyErr := os.Stat(keyPath)
	switch {
	case errors.Is(certErr, fs.ErrNotExist) && errors.Is(keyErr, fs.ErrNotExist):
		return false, nil
	case errors.Is(certErr, fs.ErrNotExist) != errors.Is(keyErr, fs.ErrNotExist):
		return false, fmt.Errorf("tls: only one of %s and %s exists; remove both so the Hub can generate a new pair", certPath, keyPath)
	}
	pair, err := loadPair(certPath, keyPath, false)
	if errors.Is(err, identity.ErrInvalidArgument) {
		return false, fmt.Errorf("tls: the generated private key %s cannot be used: %w. %s", keyPath, err, generatedKeyGuidance(filepath.Dir(keyPath)))
	}
	if err != nil {
		return false, fmt.Errorf("tls: cannot use the generated certificate %s: %w; remove the files in %s to generate a new pair", certPath, err, filepath.Dir(certPath))
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return false, fmt.Errorf("tls: cannot parse %s: %w", certPath, err)
	}
	if !now.Add(renewBefore).Before(leaf.NotAfter) {
		return false, nil
	}
	for _, n := range names {
		if leaf.VerifyHostname(n) != nil {
			return false, nil
		}
	}
	return true, nil
}

// generateLocked creates the pair while it holds the generation lock, so two
// starts on the same data dir (two containers, a restart racing a slow stop)
// never write interleaved pairs. After it got the lock it checks again: when
// another start has just published a usable pair, that pair is kept and
// generated is false.
func generateLocked(dir, certPath, keyPath string, names []string, now func() time.Time) (generated bool, err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, fmt.Errorf("tls: create %s: %w", dir, err)
	}
	// The key is created in this directory: it must be one that other local
	// users cannot write, or they could swap the key under the Hub.
	if err := checkKeyDir(dir); err != nil {
		return false, fmt.Errorf("tls: the private key cannot be created in %s: %w. %s", dir, err, generatedKeyGuidance(dir))
	}
	unlock, err := lockDir(dir)
	if err != nil {
		return false, err
	}
	defer unlock()
	usable, err := existingUsable(certPath, keyPath, names, now())
	if err != nil {
		return false, err
	}
	if usable {
		return false, nil
	}
	if err := generateSelfSigned(dir, certPath, keyPath, names, now()); err != nil {
		return false, err
	}
	return true, nil
}

const (
	lockName       = ".generate.lock"
	lockWait       = 15 * time.Second
	lockStaleAfter = 60 * time.Second
)

// processLock serialises generation inside one process; the lock file does it
// across processes.
var processLock sync.Mutex

// lockDir takes the generation lock of dir: an exclusively created file. A
// lock older than lockStaleAfter belongs to a process that died and is taken
// over.
func lockDir(dir string) (unlock func(), err error) {
	processLock.Lock()
	path := filepath.Join(dir, lockName)
	deadline := time.Now().Add(lockWait)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			return func() { _ = os.Remove(path); processLock.Unlock() }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			processLock.Unlock()
			return nil, fmt.Errorf("tls: lock %s: %w", path, err)
		}
		if fi, statErr := os.Stat(path); statErr == nil && time.Since(fi.ModTime()) > lockStaleAfter {
			_ = os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			processLock.Unlock()
			return nil, fmt.Errorf("tls: another Hub start holds %s; if no other Hub is starting, remove that file", path)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func generateSelfSigned(dir, certPath, keyPath string, names []string, now time.Time) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("tls: generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return fmt.Errorf("tls: generate serial number: %w", err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Minion Hub", Organization: []string{"Minion"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(selfSignedValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	addSANs(tpl, names)
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("tls: create certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("tls: encode key: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("tls: create %s: %w", dir, err)
	}
	return publishPair(certPath, keyPath,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
}

// renameFile moves a staged file into place. Tests replace it to simulate a
// failure between the two renames of [publishPair].
var renameFile = os.Rename

// publishPair puts a new certificate and key in place without ever losing
// the pair that is there. Both files are fully written and synced under
// temporary names first, so no failure of the writes touches the old pair.
// Only then are they renamed, the key first. When the second rename fails the
// previous key is put back, so the old certificate keeps matching its key
// (and a first-time failure leaves nothing behind). Only a crash exactly
// between the two renames can leave a mismatched pair; the next start then
// reports it and says to remove the files.
func publishPair(certPath, keyPath string, certPEM, keyPEM []byte) error {
	keyTmp, err := stageFile(keyPath, keyPEM, 0o600, true)
	if err != nil {
		return err
	}
	certTmp, err := stageFile(certPath, certPEM, 0o644, false)
	if err != nil {
		_ = os.Remove(keyTmp)
		return err
	}
	// The key that is being replaced is read through the same protected open
	// as every other use of a key, so a file swapped in now is not copied back.
	oldKey, readErr := readPrivateKey(keyPath, false)
	hadKey := readErr == nil
	if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		_ = os.Remove(keyTmp)
		_ = os.Remove(certTmp)
		return fmt.Errorf("tls: cannot replace %s: %w", keyPath, readErr)
	}
	if err := renameFile(keyTmp, keyPath); err != nil {
		_ = os.Remove(keyTmp)
		_ = os.Remove(certTmp)
		return fmt.Errorf("tls: write %s: %w", keyPath, err)
	}
	if err := renameFile(certTmp, certPath); err != nil {
		_ = os.Remove(certTmp)
		if hadKey {
			if restoreErr := writeFileAtomic(keyPath, oldKey, 0o600, true); restoreErr != nil {
				err = errors.Join(err, restoreErr)
			}
		} else {
			_ = os.Remove(keyPath)
		}
		return fmt.Errorf("tls: write %s: %w", certPath, err)
	}
	return nil
}

// addSANs puts the names a browser or client may use into the certificate:
// localhost, loopback, this machine's host name and the extra names.
func addSANs(tpl *x509.Certificate, extra []string) {
	seen := map[string]bool{}
	add := func(n string) {
		n = strings.ToLower(strings.Trim(strings.TrimSpace(n), "[]"))
		if n == "" || seen[n] {
			return
		}
		seen[n] = true
		if ip := net.ParseIP(n); ip != nil {
			tpl.IPAddresses = append(tpl.IPAddresses, ip)
			return
		}
		tpl.DNSNames = append(tpl.DNSNames, n)
	}
	add("localhost")
	add("127.0.0.1")
	add("::1")
	if h, err := os.Hostname(); err == nil {
		add(h)
	}
	for _, n := range extra {
		add(n)
	}
}

// stageFile writes data to a synced temporary file next to path and returns
// its name. The caller renames it into place or removes it. With private the
// file is created private from the start (see [identity.CreatePrivateTemp]:
// mode 0600 on Unix, a protected owner-and-SYSTEM-only DACL on Windows), so a
// private key never exists with wider access, not even for a moment.
func stageFile(path string, data []byte, mode os.FileMode, private bool) (string, error) {
	create := os.CreateTemp
	if private {
		create = identity.CreatePrivateTemp
	}
	tmp, err := create(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return "", fmt.Errorf("tls: write %s: %w", path, err)
	}
	name := tmp.Name()
	fail := func(err error) (string, error) {
		_ = tmp.Close()
		_ = os.Remove(name)
		return "", fmt.Errorf("tls: write %s: %w", path, err)
	}
	if !private {
		if err := tmp.Chmod(mode); err != nil && !errors.Is(err, errors.ErrUnsupported) {
			return fail(err)
		}
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return "", fmt.Errorf("tls: write %s: %w", path, err)
	}
	return name, nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode, private bool) error {
	name, err := stageFile(path, data, mode, private)
	if err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("tls: write %s: %w", path, err)
	}
	return nil
}

// Size bounds of the PEM files. A key or certificate chain is a few kilobytes;
// anything larger is a wrong path.
const (
	maxKeyFileSize  = 64 << 10
	maxCertFileSize = 1 << 20
)

// loadPair reads the certificate and the private key and parses exactly the
// bytes that were read and checked (tls.X509KeyPair), never a second read of
// the files by name. The key is opened through [readPrivateKey]: it must be a
// protected private file. The certificate is public and read as it is.
// explicit is true for a pair the operator supplied
// (MINION_TLS_CERT_FILE and MINION_TLS_KEY_FILE).
func loadPair(certPath, keyPath string, explicit bool) (tls.Certificate, error) {
	keyPEM, err := readPrivateKey(keyPath, explicit)
	if err != nil {
		return tls.Certificate{}, err
	}
	certPEM, err := readLimitedFile(certPath, maxCertFileSize)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

// readPrivateKey reads the private key file at path under the policy of the
// other Hub secrets (the data key, see store.LoadOrCreateKey, and the identity
// keys): the directory it lies in must not be writable by other local users or
// owned by one, and the file must be a regular file owned by the user that runs
// the Hub with no access for group or others (Windows: a protected DACL that
// grants nobody else access). The check is made on the handle that is read (see
// [identity.OpenPrivate]), so the bytes parsed are the bytes checked. An unsafe
// key is refused and never changed: a key that others could read may already
// have been read, so tightening its mode quietly would only hide that. The error
// never contains key material.
//
// For an operator-supplied key (explicit) a symbolic link is followed, because
// certificate tooling and secret mounts publish keys that way (for example
// /etc/letsencrypt/live/<name>/privkey.pem): the directory of the link and the
// directory of its target must both pass the directory check, and the target
// the file check, so every hop is in a directory only the Hub user or root can
// change. The generated key is never a link.
func readPrivateKey(path string, explicit bool) ([]byte, error) {
	target := path
	if explicit {
		if real, err := filepath.EvalSymlinks(path); err == nil {
			target = real
		}
	}
	dirs := []string{filepath.Dir(path)}
	if target != path {
		dirs = append(dirs, filepath.Dir(target))
	}
	for _, d := range dirs {
		if err := identity.CheckPrivateDir(d); err != nil {
			return nil, err
		}
	}
	f, err := identity.OpenPrivate(target)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxKeyFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) > maxKeyFileSize {
		return nil, fmt.Errorf("%s is larger than %d bytes; it is not a private key", path, maxKeyFileSize)
	}
	return data, nil
}

// readLimitedFile reads at most limit bytes of the public file path.
func readLimitedFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, limit)
	}
	return data, nil
}

// checkKeyDir refuses a directory that another local user could use to replace
// the generated key. A directory that does not exist is not an error here: it
// is created private first.
func checkKeyDir(dir string) error {
	err := identity.CheckPrivateDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// generatedKeyGuidance tells the operator what to do with a generated key that
// is not safe. The Hub never repairs, overwrites or regenerates it on its own.
func generatedKeyGuidance(dir string) string {
	return "The Hub will not use it, and nothing was changed: a key that other users could read or replace may already have been read, " +
		"so tightening its mode quietly would only hide that. " +
		"If only you ever had access to it (for example you just restored it yourself), make the files in " + dir +
		" plain files owned by the Hub user (chmod 600 on the key) in directories that only that user can write, and start again. " +
		"If anyone else may have had access, stop the Hub, remove the files in " + dir +
		" so that the Hub generates a new pair, and do not trust the old certificate any more"
}

// Reload re-reads the certificate and key files. On failure the previous
// certificate stays in service.
func (c *CertSource) Reload() error {
	pair, err := loadPair(c.certFile, c.keyFile, !c.SelfSigned)
	if err != nil {
		return fmt.Errorf("tls: load certificate %s and key %s: %w", c.certFile, c.keyFile, err)
	}
	if len(pair.Certificate) == 0 {
		return fmt.Errorf("tls: %s holds no certificate", c.certFile)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return fmt.Errorf("tls: parse certificate %s: %w", c.certFile, err)
	}
	pair.Leaf = leaf
	c.cur.Store(&pair)
	return nil
}

// Certificate returns the certificate in service.
func (c *CertSource) Certificate() *tls.Certificate { return c.cur.Load() }

// GetCertificate implements tls.Config.GetCertificate.
func (c *CertSource) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return c.cur.Load(), nil
}

// Fingerprint returns the SHA-256 fingerprint of the certificate in service
// as colon-separated upper-case hex, the form browsers show.
func (c *CertSource) Fingerprint() string { return CertFingerprint(c.cur.Load().Certificate[0]) }

// CertFingerprint returns the SHA-256 fingerprint of a DER certificate as
// colon-separated upper-case hex.
func CertFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	parts := make([]string, 0, len(sum))
	for i := 0; i < len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	return strings.Join(parts, ":")
}

// TLSConfig returns the listener configuration: TLS 1.3 or newer, the
// certificate from c.
func (c *CertSource) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion:     tls.VersionTLS13,
		GetCertificate: c.GetCertificate,
		NextProtos:     []string{"h2", "http/1.1"},
	}
}

// certNames returns the extra host names for the generated certificate: the
// host of the public URL and the listen host when it is a specific address.
func certNames(publicURL, listen string) []string {
	var out []string
	if u, err := url.Parse(publicURL); err == nil && u.Hostname() != "" {
		out = append(out, u.Hostname())
	}
	if h, _, err := net.SplitHostPort(listen); err == nil {
		if ip := net.ParseIP(h); ip != nil && !ip.IsUnspecified() {
			out = append(out, h)
		} else if ip == nil && h != "" {
			out = append(out, h)
		}
	}
	return out
}

// tlsListener wraps ln so that accepted connections speak TLS with cfg.
func tlsListener(ln net.Listener, cfg *tls.Config) net.Listener { return tls.NewListener(ln, cfg) }
