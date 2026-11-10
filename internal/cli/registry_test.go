package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestRegisteredCommandAppearsUnderRoot(t *testing.T) {
	var reg Registry
	reg.Add(func(root *cobra.Command) {
		root.AddCommand(&cobra.Command{Use: "example", Run: func(*cobra.Command, []string) {}})
	})

	root := newRoot(&reg)
	found := false
	for _, c := range root.Commands() {
		if c.Name() == "example" {
			found = true
		}
	}
	if !found {
		t.Fatal("registered command \"example\" is not under the root command")
	}
}

func TestRegistrationOrderIsKept(t *testing.T) {
	var reg Registry
	var order []string
	for _, n := range []string{"a", "b", "c"} {
		reg.Add(func(*cobra.Command) { order = append(order, n) })
	}
	reg.Apply(&cobra.Command{Use: "minion"})
	if got := strings.Join(order, ""); got != "abc" {
		t.Fatalf("order = %q, want %q", got, "abc")
	}
}

func TestRegisterNilPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Add(nil) did not panic")
		}
	}()
	var reg Registry
	reg.Add(nil)
}

func TestNewRootBuildsIndependentTrees(t *testing.T) {
	a, b := NewRoot(), NewRoot()
	if a == b {
		t.Fatal("NewRoot returned the same command twice")
	}
}

func TestPackageRegisterReachesNewRoot(t *testing.T) {
	// The package-level Register feeds NewRoot. Register a probe command and
	// check that the next root contains it next to the built-in ones.
	Register(func(root *cobra.Command) {
		root.AddCommand(&cobra.Command{Use: "registry-probe", Run: func(*cobra.Command, []string) {}})
	})
	names := map[string]bool{}
	for _, c := range NewRoot().Commands() {
		names[c.Name()] = true
	}
	for _, want := range []string{"registry-probe", "version"} {
		if !names[want] {
			t.Errorf("command %q missing under root", want)
		}
	}
}

func TestExecuteUnknownCommandFails(t *testing.T) {
	var out, errb bytes.Buffer
	code := Execute(context.Background(), []string{"no-such-command"}, &out, &errb)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errb.String(), "Error:") {
		t.Fatalf("stderr = %q, want an error line", errb.String())
	}
}
