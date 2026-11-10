package proto

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"unicode/utf8"
)

// Frame size limits (AD-3). The limit is the maximum length of the JSON body,
// not counting the 4-byte length prefix.
const (
	// HandshakeFrameLimit is the largest handshake frame body: 64 KiB.
	HandshakeFrameLimit = 64 << 10
	// StreamFrameLimit is the largest frame body on a yamux stream: 1 MiB.
	StreamFrameLimit = 1 << 20
)

// frameHeaderLen is the size of the big-endian uint32 length prefix.
const frameHeaderLen = 4

// WriteFrame encodes v as one UTF-8 JSON object with a non-empty string "type"
// field and writes it to w as a uint32 big-endian length followed by the JSON
// bytes, in a single Write call. v is anything encoding/json turns into such
// an object (a struct, a map or a json.RawMessage). Nothing is written when
// the encoded frame is larger than limit (frame_too_large) or is not a UTF-8
// object with a type field (invalid_argument). A json.RawMessage is re-encoded
// by encoding/json, so insignificant whitespace is dropped and <, > and & are
// escaped; the size limit applies to the bytes actually written.
func WriteFrame(w io.Writer, limit int, v any) error {
	body, err := encodeBody(v)
	if err != nil {
		return err
	}
	if err := checkSize(len(body), limit); err != nil {
		return err
	}
	buf := make([]byte, frameHeaderLen+len(body))
	binary.BigEndian.PutUint32(buf, uint32(len(body)))
	copy(buf[frameHeaderLen:], body)
	if _, err := w.Write(buf); err != nil {
		return err
	}
	return nil
}

// ReadFrame reads one frame from r and returns its JSON body, validated to be
// a UTF-8 JSON object with a non-empty string "type" field (see frameType). A
// length above limit is
// rejected with frame_too_large before the body is read. io.EOF is returned
// only when r ends cleanly before the first byte of a frame; a frame cut short
// yields io.ErrUnexpectedEOF. A zero length or a body that is not a typed JSON
// object yields invalid_argument.
func ReadFrame(r io.Reader, limit int) (json.RawMessage, error) {
	var hdr [frameHeaderLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err // io.EOF on a clean end, io.ErrUnexpectedEOF mid-prefix
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if uint64(n) > uint64(maxInt(limit)) {
		return nil, tooLarge(uint64(n), limit)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	if _, err := frameType(body); err != nil {
		return nil, err
	}
	return json.RawMessage(body), nil
}

// FrameType returns the "type" field of an encoded frame body.
func FrameType(body []byte) (string, error) { return frameType(body) }

// DecodeFrame unmarshals a frame body returned by ReadFrame into v. Unknown
// fields are ignored, as AD-4 requires of receivers.
func DecodeFrame(body []byte, v any) error {
	if err := json.Unmarshal(body, v); err != nil {
		return NewError(CodeInvalidArgument, "frame body does not match the expected shape: "+err.Error())
	}
	return nil
}

func encodeBody(v any) ([]byte, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, NewError(CodeInvalidArgument, "frame is not encodable as JSON: "+err.Error())
	}
	if _, err := frameType(body); err != nil {
		return nil, err
	}
	return body, nil
}

// frameType validates body as one UTF-8 JSON object with a non-empty string
// "type" member and returns that member. Leading and trailing JSON whitespace
// is accepted. The key is matched exactly (case-sensitively), unlike
// encoding/json struct decoding, so a member such as "TYPE" is an unknown
// field (AD-4) and never decides the frame type. A body with two "type"
// members is rejected because receivers could disagree on which one wins.
func frameType(body []byte) (string, error) {
	if !utf8.Valid(body) {
		return "", NewError(CodeInvalidArgument, "frame body is not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	tok, err := dec.Token()
	if err == io.EOF {
		return "", NewError(CodeInvalidArgument, "frame body is not a JSON object")
	}
	if err != nil {
		return "", NewError(CodeInvalidArgument, "frame body is not valid JSON: "+err.Error())
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return "", NewError(CodeInvalidArgument, "frame body is not a JSON object")
	}
	var typeRaw json.RawMessage
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return "", NewError(CodeInvalidArgument, "frame body is not valid JSON: "+err.Error())
		}
		key, _ := keyTok.(string) // the next token inside an object is always a string key
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return "", NewError(CodeInvalidArgument, "frame body is not valid JSON: "+err.Error())
		}
		if key == "type" {
			if typeRaw != nil {
				return "", NewError(CodeInvalidArgument, "frame body has more than one type field")
			}
			typeRaw = val
		}
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return "", NewError(CodeInvalidArgument, "frame body is not valid JSON: "+err.Error())
	}
	if _, err := dec.Token(); err != io.EOF {
		return "", NewError(CodeInvalidArgument, "frame body is not valid JSON: data after the top-level value")
	}
	if typeRaw == nil {
		return "", NewError(CodeInvalidArgument, "frame body has no type field")
	}
	var typ string
	if err := json.Unmarshal(typeRaw, &typ); err != nil || typeRaw[0] == 'n' {
		return "", NewError(CodeInvalidArgument, "frame body type field is not a string")
	}
	if typ == "" {
		return "", NewError(CodeInvalidArgument, "frame body has no type field")
	}
	return typ, nil
}

func checkSize(n, limit int) error {
	if n < 0 || n > maxInt(limit) {
		return tooLarge(uint64(n), limit)
	}
	return nil
}

func tooLarge(n uint64, limit int) error {
	return NewError(CodeFrameTooLarge, fmt.Sprintf("frame of %d bytes exceeds the limit of %d bytes", n, maxInt(limit)))
}

func maxInt(limit int) int {
	if limit < 0 {
		return 0
	}
	return limit
}
