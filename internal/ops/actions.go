package ops

import (
	"fmt"
	"slices"
	"strings"
)

// Action is a permission verb a caller can hold on a resource (AD-11). An
// operation accepts a set of alternative actions, any one of which suffices.
type Action string

// The release-1 actions.
const (
	// ActionRead reads state: lists, snapshots, captures, pipe turns.
	ActionRead Action = "read"
	// ActionOperate changes state of a Worker or session: typing, keys, tasks.
	ActionOperate Action = "operate"
	// ActionMessage delivers messages to agent sessions only (pipe).
	ActionMessage Action = "message"
	// ActionManage is the global administrative action.
	ActionManage Action = "manage"
)

// Valid reports whether a is one of the four known actions.
func (a Action) Valid() bool {
	switch a {
	case ActionRead, ActionOperate, ActionMessage, ActionManage:
		return true
	}
	return false
}

// ActionSet is the accepted action set of an operation: the alternatives any
// one of which suffices (for example pipe_send accepts message|operate). A
// valid set is non-empty, duplicate free and sorted, which makes its string
// form canonical.
type ActionSet []Action

// NewActionSet validates and canonicalises the alternatives. It returns an
// error for an empty set, an unknown action or a duplicate.
func NewActionSet(actions ...Action) (ActionSet, error) {
	if len(actions) == 0 {
		return nil, fmt.Errorf("ops: accepted action set is empty")
	}
	set := slices.Clone(actions)
	slices.Sort(set)
	for i, a := range set {
		if !a.Valid() {
			return nil, fmt.Errorf("ops: unknown action %q", a)
		}
		if i > 0 && set[i-1] == a {
			return nil, fmt.Errorf("ops: duplicate action %q in accepted action set", a)
		}
	}
	return ActionSet(set), nil
}

// MustActionSet is NewActionSet for package-level declarations; it panics on
// an invalid set.
func MustActionSet(actions ...Action) ActionSet {
	set, err := NewActionSet(actions...)
	if err != nil {
		panic(err)
	}
	return set
}

// ParseActionSet parses the canonical string form "a|b".
func ParseActionSet(s string) (ActionSet, error) {
	parts := strings.Split(s, "|")
	actions := make([]Action, len(parts))
	for i, p := range parts {
		actions[i] = Action(p)
	}
	return NewActionSet(actions...)
}

// String returns the canonical form: the sorted alternatives joined by "|".
func (s ActionSet) String() string {
	parts := make([]string, len(s))
	for i, a := range s {
		parts[i] = string(a)
	}
	return strings.Join(parts, "|")
}

// Permits reports whether at least one held action is in the accepted set.
// Actions are alternatives and do not imply each other: manage does not grant
// operate.
func (s ActionSet) Permits(held ...Action) bool {
	for _, h := range held {
		if slices.Contains(s, h) {
			return true
		}
	}
	return false
}
