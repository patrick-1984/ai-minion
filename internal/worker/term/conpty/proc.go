package conpty

import "io"

// proc is one program running in a pseudo console, as the host sees it. The
// Windows implementation wraps go-pty; tests use an in-memory fake.
type proc interface {
	// Read returns the pseudo console output. It fails once the pseudo
	// console is closed.
	io.Reader
	// Write sends input to the program.
	io.Writer
	// Pid is the process id of the program.
	Pid() int
	// Wait blocks until the program has exited.
	Wait() error
	// Kill ends the program.
	Kill() error
	// Close closes the pseudo console, which ends Read.
	Close() error
}

// startSpec describes the program a new session runs.
type startSpec struct {
	path       string   // program, as given or resolved
	args       []string // arguments without the program
	dir        string   // working directory, empty for the Worker's
	cols, rows int      // initial size of the pseudo console
}

// starter starts a program in a new pseudo console.
type starter func(spec startSpec) (proc, error)
