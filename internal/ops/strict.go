package ops

import "strings"

// Strict operations refuse what they do not know: an argument member that
// the operation does not declare is invalid_argument, in a POST body and in a
// GET query alike, instead of being ignored. The operations that existed
// before the set keep the additive rule (unknown members are ignored), so no
// existing client breaks; every new operation family starts strict.
//
// The Hub also de-duplicates the Hub-scope writes of the set by their
// request_id (a repeated request is answered from the stored result). An
// operation whose result holds a one-time secret must not join the set
// before it has a replay rule of its own.

// strictPrefixes names the operation families that are strict as a whole,
// future members included.
var strictPrefixes = []string{"grant_", "access_", "file_"}

// strictOps names the strict operations one by one. The prefixes cover them
// too; the names are listed so that the set is readable in one place.
var strictOps = map[string]struct{}{
	OpGrantList:     {},
	OpGrantSet:      {},
	OpGrantRevoke:   {},
	OpGrantApprove:  {},
	OpAccessMatrix:  {},
	OpAccessExplain: {},
	OpGrantBind:     {},
	OpGrantHistory:  {},

	OpFileList:          {},
	OpFilePull:          {},
	OpFilePush:          {},
	OpFileReceipt:       {},
	OpFileTransfersList: {},
}

// StrictArgs reports whether the operation name refuses unknown argument
// members.
func StrictArgs(name string) bool {
	if _, ok := strictOps[name]; ok {
		return true
	}
	for _, p := range strictPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
