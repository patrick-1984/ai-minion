//go:build !unix

package cli

import "errors"

func syscallMkfifo(string) error { return errors.New("no FIFO") }
