package agentdetect

import "github.com/patrick-1984/ai-minion/internal/worker/term"

// MarkReason returns the reason that a delivery mark cannot anchor a turn
// (one of the Reason constants: tmux_too_old, no_evidence, alternate_screen,
// scroll_region), or "" when the delivery can be followed. It is the check
// Delivered makes on the mark, for a caller that wants to tell the sender at
// once that no reply can be extracted (Story 7.3).
func MarkReason(m term.DeliveryMark) string { return markReason(m) }
