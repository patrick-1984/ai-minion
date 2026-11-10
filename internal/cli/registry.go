package cli

import (
	"sync"

	"github.com/spf13/cobra"
)

// RegisterFunc attaches commands to a root command.
type RegisterFunc func(root *cobra.Command)

// Registry collects RegisterFuncs and applies them to a root command in
// registration order. The zero value is ready to use.
type Registry struct {
	mu    sync.Mutex
	funcs []RegisterFunc
}

// Add records fn. It panics on a nil function, because that is a programming
// error that would otherwise surface only when the command tree is built.
func (r *Registry) Add(fn RegisterFunc) {
	if fn == nil {
		panic("cli: Register called with a nil function")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.funcs = append(r.funcs, fn)
}

// Apply calls every recorded function on root, in registration order.
func (r *Registry) Apply(root *cobra.Command) {
	r.mu.Lock()
	funcs := append([]RegisterFunc(nil), r.funcs...)
	r.mu.Unlock()
	for _, fn := range funcs {
		fn(root)
	}
}

// defaultRegistry is the registry used by Register and NewRoot.
var defaultRegistry Registry

// Register adds a command registration to the default registry. A story adds
// a command by calling Register from an init function in its own file:
//
//	func init() {
//		cli.Register(func(root *cobra.Command) {
//			root.AddCommand(&cobra.Command{Use: "example", RunE: runExample})
//		})
//	}
//
// Go initialises the files of a package in file-name order, so the order of
// commands is deterministic.
func Register(fn RegisterFunc) {
	defaultRegistry.Add(fn)
}
