package tmux

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// MasterPane identifies one presentation of a pane on this host. Linked
// windows have one row per session, even when the pane IDs and PIDs match.
type MasterPane struct {
	ServerPID   int
	PanePID     int
	PaneID      string
	SessionID   string
	SessionName string
	WindowID    string
}

// MasterPanes reads identity metadata only, without looking at terminal
// output, command lines, or the caller's environment.
func (h *Host) MasterPanes(ctx context.Context) ([]MasterPane, error) {
	if h.psmux || h.userProbe {
		return nil, fmt.Errorf("this terminal host does not support master attachments")
	}
	codec, err := newPaneCodec()
	if err != nil {
		return nil, err
	}
	format := strings.Join([]string{
		"#{pid}", "#{pane_pid}", "#{pane_id}", "#{session_id}", "#{session_name}", "#{window_id}",
	}, codec.fs) + codec.rs
	data, err := h.run(ctx, "list-panes", "-a", "-F", format)
	if err != nil {
		return nil, err
	}
	rows := strings.Split(string(data), codec.rs+"\n")
	if rows[len(rows)-1] != "" {
		return nil, fmt.Errorf("incomplete terminal identity listing")
	}
	panes := make([]MasterPane, 0, len(rows)-1)
	for _, row := range rows[:len(rows)-1] {
		parts := strings.Split(row, codec.fs)
		if len(parts) != 6 || parts[2] == "" || parts[3] == "" || parts[5] == "" {
			return nil, fmt.Errorf("invalid terminal identity listing")
		}
		server, serverErr := strconv.Atoi(parts[0])
		pid, pidErr := strconv.Atoi(parts[1])
		if serverErr != nil || pidErr != nil || server <= 0 || pid <= 0 {
			return nil, fmt.Errorf("invalid terminal process identity")
		}
		panes = append(panes, MasterPane{ServerPID: server, PanePID: pid, PaneID: parts[2],
			SessionID: parts[3], SessionName: parts[4], WindowID: parts[5]})
	}
	return panes, nil
}
