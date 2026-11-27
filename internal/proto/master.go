package proto

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	TypeMasterOpen   = "master.open"
	TypeMasterReport = "master.report"
	TypeMasterState  = "master.state"
	OpMasterCallV1   = "master_call_v1"

	MasterAdapterClaude = "claude-code-linux-tmux"
	MasterCatalogActive = "active"
	MasterCatalogEmpty  = "empty"
	MasterCatalogFailed = "failed"

	MaxMasterCandidates  = 128
	MaxMasterSessionName = 128
	MaxMasterGeneration  = uint64(math.MaxInt64)
	MasterFrameLimit     = 128 << 10
	MasterLeaseTTL       = 30 * time.Second
	MasterReportInterval = 10 * time.Second
)

// MasterCandidate describes a verified shared terminal and harness attachment.
// Its catalog result is the last state reported by the integration, independent
// of the Hub's authority or of what the model keeps in its history.
type MasterCandidate struct {
	SessionName   string `json:"session_name"`
	IncarnationID string `json:"incarnation_id"`
	Adapter       string `json:"adapter"`
	CatalogResult string `json:"catalog_result,omitempty"`
}

// MasterReport is the complete snapshot of eligible candidates on an ev lane.
// The first frame is master.open. Every later frame is master.report.
type MasterReport struct {
	Type       string            `json:"type"`
	Seq        uint64            `json:"seq"`
	Candidates []MasterCandidate `json:"candidates"`
}

// MasterBinding is a Worker-internal capability bound to one live link.
// A native MCP client may know the generation, and never receives the lease ID.
type MasterBinding struct {
	IncarnationID string `json:"incarnation_id"`
	Generation    uint64 `json:"generation"`
	LeaseID       string `json:"lease_id"`
}

// MasterState replaces the active list for one master control lane. TTLMS
// cannot extend the local deadline past the send time of its acknowledged
// report plus MasterLeaseTTL.
type MasterState struct {
	Type      string          `json:"type"`
	Serial    uint64          `json:"serial"`
	ReportSeq uint64          `json:"report_seq"`
	TTLMS     int64           `json:"ttl_ms"`
	Active    []MasterBinding `json:"active"`
}

// MasterCallArgs carry one allowlisted operation over the source Worker's
// authenticated ca stream. The Hub decodes Arguments for its operation.
type MasterCallArgs struct {
	IncarnationID string          `json:"incarnation_id"`
	Generation    uint64          `json:"generation"`
	LeaseID       string          `json:"lease_id"`
	Operation     string          `json:"operation"`
	Arguments     json.RawMessage `json:"arguments"`
}

// MasterCallRequest is the strict envelope on the private ca operation. API
// keys and name-only session identity are not members of this contract.
type MasterCallRequest struct {
	Type      string         `json:"type"`
	Op        string         `json:"op"`
	RequestID string         `json:"request_id"`
	Args      MasterCallArgs `json:"args"`
}

var masterIdentity = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

func ValidMasterIdentity(s string) bool { return masterIdentity.MatchString(s) }

func ValidMasterSessionName(s string) bool {
	return s != "" && len(s) <= MaxMasterSessionName && utf8.ValidString(s) &&
		strings.TrimSpace(s) == s && !strings.ContainsAny(s, ":.") && strings.IndexFunc(s, unicode.IsControl) < 0
}

// IsMasterOperation reports whether name is an operation of the master lane.
// The file ops (files.v1) are served there for a session caller whose
// incarnation the Hub verified (a bound master lease): the caller daemon of
// the Worker sends them, the harness's own tool catalog does not offer them.
func IsMasterOperation(name string) bool {
	switch name {
	case "workers_list", "sessions_list", "session_snapshot", "send_text",
		"file_list", "file_pull", "file_push":
		return true
	default:
		return false
	}
}

func (c MasterCandidate) Validate() error {
	if !ValidMasterSessionName(c.SessionName) || !ValidMasterIdentity(c.IncarnationID) || c.Adapter != MasterAdapterClaude {
		return NewError(CodeInvalidArgument, "invalid master candidate")
	}
	switch c.CatalogResult {
	case "", MasterCatalogActive, MasterCatalogEmpty, MasterCatalogFailed:
		return nil
	default:
		return NewError(CodeInvalidArgument, "invalid master catalog result")
	}
}

func (r MasterReport) Validate() error {
	if (r.Type != TypeMasterOpen && r.Type != TypeMasterReport) || r.Seq == 0 || r.Candidates == nil || len(r.Candidates) > MaxMasterCandidates {
		return NewError(CodeInvalidArgument, "invalid master candidate snapshot")
	}
	names := make(map[string]bool, len(r.Candidates))
	incarnations := make(map[string]bool, len(r.Candidates))
	for _, c := range r.Candidates {
		if err := c.Validate(); err != nil {
			return err
		}
		if names[c.SessionName] || incarnations[c.IncarnationID] {
			return NewError(CodeInvalidArgument, "duplicate master candidate")
		}
		names[c.SessionName], incarnations[c.IncarnationID] = true, true
	}
	return nil
}

func (b MasterBinding) Validate() error {
	if !ValidMasterIdentity(b.IncarnationID) || !ValidMasterIdentity(b.LeaseID) || b.Generation == 0 || b.Generation > MaxMasterGeneration {
		return NewError(CodeInvalidArgument, "invalid master attachment")
	}
	return nil
}

func (s MasterState) Validate() error {
	if s.Type != TypeMasterState || s.Serial == 0 || s.ReportSeq == 0 || s.TTLMS <= 0 || s.TTLMS > MasterLeaseTTL.Milliseconds() ||
		s.Active == nil || len(s.Active) > MaxMasterCandidates {
		return NewError(CodeInvalidArgument, "invalid master state")
	}
	incarnations := make(map[string]bool, len(s.Active))
	leases := make(map[string]bool, len(s.Active))
	for _, b := range s.Active {
		if err := b.Validate(); err != nil {
			return err
		}
		if incarnations[b.IncarnationID] || leases[b.LeaseID] {
			return NewError(CodeInvalidArgument, "duplicate master attachment")
		}
		incarnations[b.IncarnationID], leases[b.LeaseID] = true, true
	}
	return nil
}

func (a MasterCallArgs) Validate() error {
	if err := (MasterBinding{IncarnationID: a.IncarnationID, Generation: a.Generation, LeaseID: a.LeaseID}).Validate(); err != nil {
		return err
	}
	if !IsMasterOperation(a.Operation) {
		return NewError(CodeForbidden, "operation is outside the master catalog")
	}
	if raw := bytes.TrimSpace(a.Arguments); len(raw) == 0 || len(raw) > MasterFrameLimit || raw[0] != '{' || !json.Valid(raw) {
		return NewError(CodeInvalidArgument, "expected master operation arguments")
	}
	return nil
}

func (r MasterCallRequest) Validate() error {
	if r.Type != TypeOpRequest || r.Op != OpMasterCallV1 || !ValidMasterIdentity(r.RequestID) {
		return NewError(CodeInvalidArgument, "invalid master call envelope")
	}
	return r.Args.Validate()
}

// DecodeMasterJSON is deliberately stricter than the general protocol
// decoder. This lane rejects unknown or repeated members and only decodes one
// complete object. Errors never echo the private payload.
func DecodeMasterJSON(body []byte, v any) error {
	raw := bytes.TrimSpace(body)
	invalid := func() error { return NewError(CodeInvalidArgument, "invalid master protocol object") }
	if len(raw) == 0 || len(raw) > MasterFrameLimit || raw[0] != '{' || !utf8.Valid(raw) {
		return invalid()
	}
	check := json.NewDecoder(bytes.NewReader(raw))
	if !masterJSONValue(check, 0) {
		return invalid()
	}
	if _, err := check.Token(); err != io.EOF {
		return invalid()
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return invalid()
	}
	return nil
}

func masterJSONValue(dec *json.Decoder, depth int) bool {
	if depth > 16 {
		return false
	}
	token, err := dec.Token()
	if err != nil {
		return false
	}
	switch token {
	case json.Delim('{'):
		seen := map[string]bool{}
		for dec.More() {
			name, err := dec.Token()
			key, ok := name.(string)
			folded := strings.ToLower(key)
			if err != nil || !ok || seen[folded] || !masterJSONValue(dec, depth+1) {
				return false
			}
			seen[folded] = true
		}
		last, err := dec.Token()
		return err == nil && last == json.Delim('}')
	case json.Delim('['):
		for dec.More() {
			if !masterJSONValue(dec, depth+1) {
				return false
			}
		}
		last, err := dec.Token()
		return err == nil && last == json.Delim(']')
	default:
		_, delim := token.(json.Delim)
		return !delim
	}
}
