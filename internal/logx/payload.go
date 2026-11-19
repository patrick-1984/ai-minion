package logx

import (
	"bytes"
	"encoding/json"
	"strconv"
	"unicode/utf8"
)

// PayloadKey is the attribute that carries the body of a traffic frame (the
// payload level of the Worker traffic log). [RemoteSafe] lets this one string
// attribute reach [RemotePayloadMax] bytes instead of [RemoteMaxString]; the
// producer ([RedactPayload]) has already made it valid JSON within that size.
const PayloadKey = "payload"

// RemotePayloadMax caps the payload attribute of a remote record: it leaves
// room for the metadata attributes inside [RemoteMaxRecord].
const RemotePayloadMax = 12 << 10

// maxPayloadParse bounds the frame body that is parsed; a larger body is
// reported by its size only.
const maxPayloadParse = 8 << 20

// RedactPayload turns the JSON body of a frame into the text of a traffic
// record: every member whose name is a secret key is replaced at any depth,
// every string is masked as [RedactText] does (Minion's key formats, URL user
// information and queries, pairing codes), and the result is compact JSON of
// at most max bytes. A body that does not fit becomes
// {"truncated":N,"head":"<the first bytes of the masked JSON>"}, where N is
// the number of bytes left out; a body that is not JSON becomes
// {"invalid_json":true,"bytes":N}. The result is always valid JSON and never
// carries a raw byte of the input.
func RedactPayload(raw []byte, max int) string {
	if len(raw) > maxPayloadParse {
		return `{"truncated":` + strconv.Itoa(len(raw)) + `}`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return `{"invalid_json":true,"bytes":` + strconv.Itoa(len(raw)) + `}`
	}
	b, err := json.Marshal(maskTree(tree))
	if err != nil {
		return `{"invalid_json":true,"bytes":` + strconv.Itoa(len(raw)) + `}`
	}
	if len(b) <= max {
		return string(b)
	}
	return truncatedPayload(string(b), max)
}

// truncatedPayload builds {"truncated":N,"head":"..."} within max bytes.
func truncatedPayload(s string, max int) string {
	keep := max - 40
	if keep < 0 {
		keep = 0
	}
	for {
		if keep > len(s) {
			keep = len(s)
		}
		for keep > 0 && !utf8.RuneStart(s[keep]) {
			keep--
		}
		head, _ := json.Marshal(s[:keep])
		out := `{"truncated":` + strconv.Itoa(len(s)-keep) + `,"head":` + string(head) + `}`
		if len(out) <= max || keep == 0 {
			if len(out) > max {
				return `{"truncated":` + strconv.Itoa(len(s)) + `}`
			}
			return out
		}
		keep -= (len(out) - max) + 1
		if keep < 0 {
			keep = 0
		}
	}
}

// capPayload keeps the payload attribute of a remote record within
// RemotePayloadMax: text that is already smaller passes, anything larger is
// replaced by a marker that is still valid JSON.
func capPayload(s string) string {
	if len(s) <= RemotePayloadMax {
		return s
	}
	return `{"truncated":` + strconv.Itoa(len(s)) + `}`
}
