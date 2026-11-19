package otel

import "github.com/patrick-1984/ai-minion/internal/ops"

func opsTarget() ops.Target { return ops.Target{WorkerID: "w1", SessionName: "s1"} }

func wa() ops.WriteArgs { return ops.WriteArgs{RequestID: ops.NewUUIDv7()} }

func sendTextArgs(t ops.Target, text string) *ops.SendTextArgs {
	return &ops.SendTextArgs{WriteArgs: wa(), Target: t, Text: text}
}
func setTaskArgs(t ops.Target, text string) *ops.SetTaskArgs {
	return &ops.SetTaskArgs{WriteArgs: wa(), Target: t, Task: text}
}
func pipeSendArgs(t ops.Target, text string) *ops.PipeSendArgs {
	return &ops.PipeSendArgs{WriteArgs: wa(), Target: t, Text: text}
}
func answerArgs(t ops.Target, text string) *ops.AnswerArgs {
	return &ops.AnswerArgs{WriteArgs: wa(), Target: t, Choice: text}
}
func snapshotArgs(t ops.Target) *ops.SessionSnapshotArgs { return &ops.SessionSnapshotArgs{Target: t} }
func pipeReadArgs(t ops.Target) *ops.PipeReadArgs        { return &ops.PipeReadArgs{Target: t} }
func okResult() ops.WriteResult                          { return ops.WriteResult{Outcome: ops.OutcomeDelivered} }
func snapshotResult(text string) ops.SessionSnapshotResult {
	return ops.SessionSnapshotResult{Pane: ops.PaneInfo{ID: "%1"}, Screen: ops.Screen{Width: 80, Height: 1, Lines: []string{text}}}
}
func pipeReadResult(text string) ops.PipeReadResult {
	return ops.PipeReadResult{Turns: []ops.PipeTurn{{TurnID: "t1", Session: "s1", Direction: ops.PipeReceived, Text: text}}}
}
