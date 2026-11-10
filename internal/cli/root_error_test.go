package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

// The terminal boundary: whatever an error carries, the operator's stderr gets
// text, never a control sequence, and an error cannot start a line that looks
// like another message.
func TestExecuteWritesOnlyTerminalSafeErrors(t *testing.T) {
	hostile := []error{
		proto.NewError(proto.CodeForbidden, "\x1b]52;c;ZXZpbA==\x07 paste"),
		fmt.Errorf("enroll: %w", proto.NewError(proto.CodeInviteExpired, "\x1b[2Jfake\nNext step: sudo")),
		errors.New("raw \x1b]0;title\x07 \u202e text \x7f \u009b"),
		errors.New("a\r\nError: forged second line"),
		errors.New("bad \xff\xfe utf8"),
		errors.New(strings.Repeat("x", 1<<20)),
	}
	for i, he := range hostile {
		root := &cobra.Command{Use: "x", SilenceUsage: true, SilenceErrors: true} // as NewRoot
		root.AddCommand(&cobra.Command{Use: "fail", RunE: func(*cobra.Command, []string) error { return he }})
		var out, errb bytes.Buffer
		code := executeRoot(context.Background(), root, []string{"fail"}, &out, &errb)
		if code != 1 {
			t.Fatalf("case %d: exit code %d", i, code)
		}
		got := errb.String()
		for _, r := range got {
			if (r < 0x20 && r != '\n') || r == 0x7f || (r >= 0x80 && r <= 0x9f) || r == 0x202e {
				t.Errorf("case %d: stderr carries U+%04X: %q", i, r, truncate(got))
			}
		}
		if strings.Count(got, "\n") != 1 || !strings.HasPrefix(got, "Error: ") || !strings.HasSuffix(got, "\n") {
			t.Errorf("case %d: stderr is not exactly one 'Error: ' line: %q", i, truncate(got))
		}
		if len(got) > 16<<10 {
			t.Errorf("case %d: stderr is %d bytes, it must be bounded", i, len(got))
		}
	}
}

func truncate(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}
