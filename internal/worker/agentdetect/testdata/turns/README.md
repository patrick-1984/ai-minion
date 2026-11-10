# Turn extraction fixtures

These screen sequences are **synthesised, not recorded**. They were written
from the screen layout of each agent (the idle, busy and dialog screens in
`../*.txt`: banner, echo of the sent message, reply marker, spinner or status
line, prompt, footer) and replay what a pane would show while an agent answers.
They were not captured from a live agent session, and the reply text is the
same for every agent on purpose, so that a difference between two agents points
at the layout and not at the content.

Story 7.2 anchors a turn by absolute row indexes, so the frames must be
physically possible: a terminal moves rows only by scrolling from row 0 and
rewrites the others in place. The frames were therefore rendered through a
small model of such a terminal (a fixed height of 16 rows, the transcript
printed from the top, the status line and the input box pinned at the bottom,
rows that no longer fit leaving through the top, the first visible row never
moving back) from the transcript of each scenario. The renderer was a one-off
script and was not kept; the files themselves are the source of truth. To add a
case, copy the closest directory and edit the frames by hand, keeping the first
visible row of each frame in `evidence.json` consistent with the scrolling.

`TestTurnFixtures` in `../../turns_test.go` replays every directory three ways:
with the evidence of tmux 3.8 (the exact reply), with `alternate_on=1` (no turn,
reason `alternate_screen`) and with the evidence of tmux before 3.8 (no turn,
reason `tmux_too_old`).

## Layout of one directory

The directory name is `<agent>_<scenario>`. The agent is one of the names of
`DefaultAgents()`; the scenarios are `short` (a short reply), `scroll` (a long
reply that scrolls the echo off the screen), `redraw` (identical frames, a
changed status line and a streamed last line) and `interrupted` (a reply that
stops early, with the agent's interruption line).

| File | Content |
| --- | --- |
| `sent.txt` | The message delivered to the pane (`Delivered`). |
| `00_delivered.txt` | The screen at delivery, before the agent reacted. It must classify as stable. |
| `NN_busy.txt`, `NN_stable.txt` | Numbered frames in order. `NN` is the frame number and the state is the one the classifier must report for that screen (checked by the test). |
| `evidence.json` | `width`, `height` and, for every frame file, `top`: the absolute index of its first visible row. The scroll between two frames is the difference of their `top` values; `history_size` is `top` (nothing is trimmed) and `history_collected` and `history_generation` are 0. |
| `want.txt` | The turn text that must come out, with exactly one turn emitted, on the first stable frame after a busy one. |

Regression cases that need exact control over a pane (a message repeated, a
reply that contains a footer phrase, a redraw in the middle of the view, a
resize, a clear) are written as code in `../../turns_test.go`,
`../../turns_probe_test.go` and `../../turns_anchor_test.go` on a fake pane
that reports the evidence as tmux 3.8 does.
