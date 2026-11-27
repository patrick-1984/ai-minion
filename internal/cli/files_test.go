package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// TestFilesPullRefusesADestinationOutsideTheFolderBeforeTheNetwork: --to ../x
// is refused with path_outside_root and no backend is even chosen (FR-4).
func TestFilesPullRefusesADestinationOutsideTheFolderBeforeTheNetwork(t *testing.T) {
	deps := opsDeps{
		local: func() client.Backend { t.Fatal("the local Worker was asked"); return nil },
		hub:   func() (client.Backend, error) { t.Fatal("the Hub was asked"); return nil, nil },
		hubSettings: func(*cobra.Command) (config.ClientHub, error) {
			t.Fatal("the Hub settings were read")
			return config.ClientHub{}, nil
		},
	}
	for _, use := range []string{"pull", ops.OpFilePull} {
		cmd := newFilesPull(deps, use, false)
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs([]string{"--target", ops.NewUUIDv7() + ":s", "--path", "a.txt", "--to", "../x"})
		err := cmd.Execute()
		if err == nil || !strings.HasPrefix(err.Error(), "path_outside_root") {
			t.Fatalf("%s: err %v, want path_outside_root", use, err)
		}
	}
}

// TestFilesCommandsReplaceTheDerivedOnes: the generic commands of the file
// ops are not derived; files ls|pull|push and the hidden aliases are there.
func TestFilesCommandsReplaceTheDerivedOnes(t *testing.T) {
	root := &cobra.Command{Use: "minion"}
	addOpCommands(root, opsDeps{}, ops.All())
	for _, name := range []string{ops.OpFileList, ops.OpFilePull, ops.OpFilePush} {
		if c, _, err := root.Find([]string{name}); err == nil && c != root {
			t.Errorf("%s has a derived command", name)
		}
	}
	if c, _, err := root.Find([]string{ops.OpFileTransfersList}); err != nil || c == root {
		t.Errorf("file_transfers_list has no command")
	}
	addFilesCommands(root, opsDeps{})
	for _, args := range [][]string{{"files", "ls"}, {"files", "pull"}, {"files", "push"}, {ops.OpFilePull}} {
		if c, _, err := root.Find(args); err != nil || c == root {
			t.Errorf("%v: no command", args)
		}
	}
	if c, _, _ := root.Find([]string{"files", "pull"}); c.Flags().Lookup("overwrite") == nil || c.Flags().Lookup("to") == nil {
		t.Error("files pull lacks --overwrite or --to")
	}
}

// TestFilesTargetNamesAWorker: a target without a Worker is a usage error.
func TestFilesTargetNamesAWorker(t *testing.T) {
	cmd := newFilesPush(opsDeps{}, "push", false)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--target", "s", "a.txt"})
	var ue *usageError
	if err := cmd.Execute(); err == nil || !asUsage(err, &ue) {
		t.Fatalf("err %v, want a usage error", err)
	}
}

func asUsage(err error, ue **usageError) bool {
	u, ok := err.(*usageError)
	*ue = u
	return ok
}
