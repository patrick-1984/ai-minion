// Package syslog sends log records to a syslog server: RFC 5424 messages
// (header, no structured data, a UTF-8 BOM and one JSON object as MSG) over
// UDP (RFC 5426, one message per datagram) or over TCP and TLS with octet
// counting (RFC 6587, the framing of RFC 5425), with optional mutual TLS.
//
// Owner: A49 PR 1 (T4, T5).
package syslog

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"time"
	"unicode/utf8"
)

// FacilityLocal0 is the facility of every message (local0, RFC 5424 §6.2.1).
const FacilityLocal0 = 16

// Header field limits of RFC 5424 §6.
const (
	maxHostname = 255
	maxAppName  = 48
	maxProcID   = 128
	maxMsgID    = 32
)

// bom is the UTF-8 byte order mark that starts an RFC 5424 MSG in UTF-8.
var bom = []byte{0xEF, 0xBB, 0xBF}

// Field is one member of the JSON object of MSG: a key and its JSON value.
type Field struct {
	Key   string
	Value json.RawMessage
}

// Formatter renders RFC 5424 messages within a byte budget.
type Formatter struct {
	// Hostname, AppName and ProcID are header fields; a value with spaces
	// or characters outside PRINTUSASCII, or one that is too long, becomes
	// "-".
	Hostname, AppName, ProcID string
	// MaxBytes is the budget of a whole message (header, BOM and MSG).
	MaxBytes int
}

// Severity maps a slog level to an RFC 5424 severity: debug 7, info 6,
// warn 4, error 3.
func Severity(l slog.Level) int {
	switch {
	case l < slog.LevelInfo:
		return 7
	case l < slog.LevelWarn:
		return 6
	case l < slog.LevelError:
		return 4
	}
	return 3
}

// levelName is the level as the local JSON log writes it.
func levelName(l slog.Level) string { return l.String() }

// HeaderField returns s when it is a valid RFC 5424 header field of at most
// max characters (PRINTUSASCII, no spaces), otherwise "-".
func HeaderField(s string, max int) string {
	if s == "" || len(s) > max {
		return "-"
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 33 || s[i] > 126 {
			return "-"
		}
	}
	return s
}

// Format returns one RFC 5424 message of at most f.MaxBytes bytes:
//
//	<PRI>1 TIMESTAMP HOSTNAME APP-NAME PROCID MSGID - BOM {"time":...,"level":...,"msg":...,...}
//
// When the JSON object does not fit, the largest fields are dropped first
// and "truncated":true is added; then msg is cut on a rune boundary. The MSG
// is always one valid JSON object in valid UTF-8.
func (f *Formatter) Format(t time.Time, level slog.Level, msgID, msg string, fields []Field) []byte {
	var head bytes.Buffer
	head.WriteByte('<')
	head.WriteString(strconv.Itoa(FacilityLocal0*8 + Severity(level)))
	head.WriteString(">1 ")
	if t.IsZero() {
		head.WriteByte('-')
	} else {
		head.WriteString(t.Format("2006-01-02T15:04:05.000000Z07:00"))
	}
	head.WriteByte(' ')
	host := HeaderField(f.Hostname, maxHostname)
	rest := " " + HeaderField(f.AppName, maxAppName) + " " + HeaderField(f.ProcID, maxProcID) + " " + HeaderField(msgID, maxMsgID) + " - "
	budget := f.MaxBytes - head.Len() - len(rest) - len(bom)
	// A long host name must not leave the MSG without room: the minimum
	// object {"truncated":true} and some of the message always fit.
	if budget-len(host) < 64 {
		host = "-"
	}
	head.WriteString(host)
	head.WriteString(rest)
	budget -= len(host)
	body := fitJSON(t, level, msg, fields, budget)
	out := make([]byte, 0, head.Len()+len(bom)+len(body))
	out = append(out, head.Bytes()...)
	out = append(out, bom...)
	return append(out, body...)
}

// fitJSON renders the MSG object within budget bytes.
func fitJSON(t time.Time, level slog.Level, msg string, fields []Field, budget int) []byte {
	b := renderObject(t, level, msg, fields, false)
	if len(b) <= budget {
		return b
	}
	// Drop the largest fields first, keeping the order of the rest.
	idx := make([]int, len(fields))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, c int) bool { return fieldSize(fields[idx[a]]) > fieldSize(fields[idx[c]]) })
	keep := make([]bool, len(fields))
	for i := range keep {
		keep[i] = true
	}
	for _, i := range idx {
		keep[i] = false
		b = renderObject(t, level, msg, kept(fields, keep), true)
		if len(b) <= budget {
			return b
		}
	}
	b = renderObject(t, level, msg, nil, true)
	if len(b) <= budget {
		return b
	}
	// Cut msg: the longest rune prefix whose object fits (binary search).
	lo, hi := 0, len(msg)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		cut := mid
		for cut > 0 && cut < len(msg) && !utf8.RuneStart(msg[cut]) {
			cut--
		}
		if len(renderObject(t, level, msg[:cut]+"…", nil, true)) <= budget {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	cut := lo
	for cut > 0 && cut < len(msg) && !utf8.RuneStart(msg[cut]) {
		cut--
	}
	if b = renderObject(t, level, msg[:cut]+"…", nil, true); len(b) <= budget {
		return b
	}
	// Not even the time and level fit: the smallest valid object.
	return []byte(`{"truncated":true}`)
}

func kept(fields []Field, keep []bool) []Field {
	out := make([]Field, 0, len(fields))
	for i, f := range fields {
		if keep[i] {
			out = append(out, f)
		}
	}
	return out
}

func fieldSize(f Field) int { return len(f.Key) + len(f.Value) + 4 }

func renderObject(t time.Time, level slog.Level, msg string, fields []Field, truncated bool) []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	first := true
	add := func(k string, v []byte) {
		if !first {
			b.WriteByte(',')
		}
		first = false
		b.Write(jsonString(k))
		b.WriteByte(':')
		b.Write(v)
	}
	if !t.IsZero() {
		add("time", jsonString(t.Format(time.RFC3339Nano)))
	}
	add("level", jsonString(levelName(level)))
	add("msg", jsonString(msg))
	for _, f := range fields {
		add(f.Key, f.Value)
	}
	if truncated {
		add("truncated", []byte("true"))
	}
	b.WriteByte('}')
	return b.Bytes()
}

// jsonString encodes s as a JSON string without HTML escaping; invalid UTF-8
// becomes U+FFFD.
func jsonString(s string) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return bytes.TrimRight(b.Bytes(), "\n")
}

// ValueJSON renders a resolved slog value as JSON. Groups become objects.
func ValueJSON(v slog.Value) json.RawMessage {
	v = v.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return jsonString(v.String())
	case slog.KindInt64:
		return json.RawMessage(strconv.FormatInt(v.Int64(), 10))
	case slog.KindUint64:
		return json.RawMessage(strconv.FormatUint(v.Uint64(), 10))
	case slog.KindFloat64:
		x := v.Float64()
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return jsonString(strconv.FormatFloat(x, 'g', -1, 64))
		}
		return json.RawMessage(strconv.FormatFloat(x, 'g', -1, 64))
	case slog.KindBool:
		return json.RawMessage(strconv.FormatBool(v.Bool()))
	case slog.KindDuration:
		return jsonString(v.Duration().String())
	case slog.KindTime:
		return jsonString(v.Time().Format(time.RFC3339Nano))
	case slog.KindGroup:
		return GroupJSON(v.Group())
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v.Any()); err != nil {
		return jsonString(v.String())
	}
	return bytes.TrimRight(b.Bytes(), "\n")
}

// GroupJSON renders attributes as one JSON object; an attribute with an
// empty key and a group value is inlined, as slog does.
func GroupJSON(as []slog.Attr) json.RawMessage {
	return ObjectJSON(Fields(as))
}

// Fields renders attributes as fields, dropping empty attributes and
// inlining groups with an empty key.
func Fields(as []slog.Attr) []Field {
	out := make([]Field, 0, len(as))
	for _, a := range as {
		v := a.Value.Resolve()
		if a.Equal(slog.Attr{}) {
			continue
		}
		if v.Kind() == slog.KindGroup {
			if len(v.Group()) == 0 {
				continue
			}
			if a.Key == "" {
				out = append(out, Fields(v.Group())...)
				continue
			}
		}
		out = append(out, Field{Key: a.Key, Value: ValueJSON(v)})
	}
	return out
}

// ObjectJSON renders fields as one JSON object.
func ObjectJSON(fs []Field) json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range fs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(jsonString(f.Key))
		b.WriteByte(':')
		b.Write(f.Value)
	}
	b.WriteByte('}')
	return b.Bytes()
}

// OctetFrame frames msg for TCP and TLS with octet counting (RFC 6587 §3.4.1,
// RFC 5425 §4.3): MSG-LEN SP SYSLOG-MSG.
func OctetFrame(msg []byte) []byte {
	out := make([]byte, 0, len(msg)+8)
	out = strconv.AppendInt(out, int64(len(msg)), 10)
	out = append(out, ' ')
	return append(out, msg...)
}
