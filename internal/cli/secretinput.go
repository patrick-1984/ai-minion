package cli

// Secret input of the CLI: a secret (the join token of "minion worker join
// --token-file PATH|-") read from a file or from stdin (tray tickets v3,
// W7b). The reader is bounded, so a wrong path or an endless pipe cannot
// exhaust memory, and it stops when its context ends (Ctrl+C, SIGTERM), so a
// pipe that never closes does not hang the command.
//
// Its errors never hold the bytes read, and a file error does not name the
// path either, as the reader of "<NAME>_FILE" variables does not: the path of
// a secret file says where the secret is, and a path given by mistake can be
// the secret itself.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

// maxSecretInput bounds what is read from a secret file or stdin: a join
// token is about 250 bytes.
const maxSecretInput = 8 << 10

// errSecretTooLong is the error of an input longer than the bound.
var errSecretTooLong = errors.New("the input is too long")

// errSecretInterrupted is the error of a read that the context ended.
var errSecretInterrupted = errors.New("interrupted while reading")

// readSecretInput reads the secret from the file at path, or from stdin for
// "-", at most limit bytes. what names the secret in the errors ("the join
// token"). The caller owns the returned bytes and clears them when done.
//
// A read the context ends returns an error that wraps errSecretInterrupted
// and ctx.Err(). The goroutine of a blocked read stays until the read
// returns (a pipe cannot be interrupted portably); it clears what it read and
// drops it.
func readSecretInput(ctx context.Context, path string, stdin io.Reader, limit int, what string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s was not read: %w (%w)", what, errSecretInterrupted, err)
	}
	var in io.Reader
	if path == "-" {
		if stdin == nil {
			return nil, fmt.Errorf("there is no stdin to read %s from", what)
		}
		in = stdin
	} else {
		f, err := openRegularSecretFile(ctx, path)
		if errors.Is(err, errSecretInterrupted) {
			return nil, fmt.Errorf("%s was not read: %w (%w)", what, errSecretInterrupted, ctx.Err())
		}
		if err != nil {
			return nil, fmt.Errorf("cannot read the file of %s: %w", what, pathlessError(err))
		}
		defer func() { _ = f.Close() }()
		in = f
	}
	raw, err := readBounded(ctx, in, limit)
	if errors.Is(err, errSecretTooLong) {
		return nil, fmt.Errorf("%s is malformed: %w; give only %s", what, err, what)
	}
	if err != nil && ctx.Err() != nil {
		return nil, fmt.Errorf("%s was not read: %w (%w)", what, errSecretInterrupted, ctx.Err())
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", what, pathlessError(err))
	}
	return raw, nil
}

// errSecretNotRegular is the error of a secret path that is not a regular
// file (a FIFO or device); use "-" to read a pipe.
var errSecretNotRegular = errors.New("not a regular file (use --token-file - to read a pipe)")

// openRegularSecretFile opens path in a goroutine that ctx can abandon (the
// open of a FIFO without a writer blocks, and Ctrl+C must still end the
// command), and accepts only a regular file.
func openRegularSecretFile(ctx context.Context, path string) (*os.File, error) {
	type result struct {
		f   *os.File
		err error
	}
	done := make(chan result, 1)
	go func() {
		f, err := os.Open(path)
		if err == nil {
			if fi, serr := f.Stat(); serr != nil || !fi.Mode().IsRegular() {
				_ = f.Close()
				f, err = nil, errSecretNotRegular
				if serr != nil {
					err = serr
				}
			}
		}
		done <- result{f, err}
	}()
	select {
	case r := <-done:
		return r.f, r.err
	case <-ctx.Done():
		go func() {
			if r := <-done; r.f != nil {
				_ = r.f.Close()
			}
		}()
		return nil, errSecretInterrupted
	}
}

// readBounded reads in to its end, at most limit bytes (errSecretTooLong
// beyond that), in a goroutine that ctx can abandon.
func readBounded(ctx context.Context, in io.Reader, limit int) ([]byte, error) {
	type result struct {
		raw []byte
		err error
	}
	// Unbuffered: a result nobody takes any more is cleared, not left in
	// the channel.
	done := make(chan result)
	stop := make(chan struct{})
	go func() {
		raw, err := io.ReadAll(io.LimitReader(in, int64(limit)+1))
		if err == nil && len(raw) > limit {
			clear(raw)
			raw, err = nil, errSecretTooLong
		}
		select {
		case done <- result{raw, err}:
		case <-stop:
			clear(raw)
		}
	}()
	select {
	case r := <-done:
		return r.raw, r.err
	case <-ctx.Done():
		close(stop)
		// The read may have ended at the same time: drop what it gave.
		select {
		case r := <-done:
			clear(r.raw)
		default:
		}
		return nil, ctx.Err()
	}
}

// pathlessError drops the path of a file error.
func pathlessError(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}
