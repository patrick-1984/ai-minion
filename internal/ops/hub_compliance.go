package ops

import "reflect"

// Names of the compliance monitoring operations (A49 design v2, §5.4).
const (
	// OpComplianceSet is the only panel path for the compliance settings. It
	// needs a direct administrator browser session whose sign-in is at most
	// five minutes old: an API key, an act-as session and a Worker are
	// refused.
	OpComplianceSet = "compliance_set"
	// OpComplianceStatus shows the policy in force and the state of the
	// compliance export.
	OpComplianceStatus = "compliance_status"
)

// ComplianceSetArgs are the arguments of compliance_set: a batch of changes
// of the compliance keys (and of worker_traffic_log, whose value payload is
// gated the same way), validated as a whole and stored in one transaction
// with the policy version and the audit rows.
type ComplianceSetArgs struct {
	// Changes lists the changes; at least one. Every key is one of
	// compliance_monitoring, compliance_input, compliance_endpoint,
	// compliance_headers, compliance_ca_file, compliance_client_cert_file,
	// compliance_client_key_file and worker_traffic_log.
	Changes []SettingChange `json:"changes"`
	// Expect lists settings the change relies on without touching them.
	Expect []SettingRevision `json:"expect,omitempty"`
}

// Validate checks the batch like settings_set does.
func (a ComplianceSetArgs) Validate() error {
	if len(a.Changes) == 0 {
		return invalid("changes", "must hold at least one change")
	}
	return SettingsSetArgs{WriteArgs: WriteArgs{RequestID: validRequestIDForBatch}, Changes: a.Changes, Expect: a.Expect}.Validate()
}

// validRequestIDForBatch lets ComplianceSetArgs reuse the batch validation of
// settings_set, whose arguments carry a request id that this op has not.
const validRequestIDForBatch = "00000000-0000-7000-8000-000000000000"

// CompliancePolicy is the policy in force.
type CompliancePolicy struct {
	// Version increases by one with every effective change.
	Version int64 `json:"version"`
	// Level is off, actions or output; Input is on or off.
	Level string `json:"level"`
	Input string `json:"input"`
	// Traffic is metadata or payload (the Worker traffic log level; the
	// payload level records frame bodies).
	Traffic string `json:"traffic"`
	// Destination is scheme://host:port of the compliance collector, empty
	// when monitoring is off.
	Destination string `json:"destination,omitempty"`
	// UpdatedAt and UpdatedBy say when and by whom the version was set.
	UpdatedAt *Timestamp `json:"updated_at,omitempty"`
	UpdatedBy string     `json:"updated_by,omitempty"`
}

// ComplianceSetResult is the result of compliance_set.
type ComplianceSetResult struct {
	Policy   CompliancePolicy `json:"policy"`
	Warnings []Warning        `json:"warnings"`
}

// ComplianceStatusArgs are the arguments of compliance_status (none).
type ComplianceStatusArgs struct{}

// ComplianceStatusResult is the result of compliance_status.
type ComplianceStatusResult struct {
	Policy CompliancePolicy `json:"policy"`
	// Export is the state of the compliance exporter; absent while monitoring
	// is off.
	Export *LogSinkStatus `json:"export,omitempty"`
}

func init() {
	manage := MustActionSet(ActionManage)
	Register(Op{
		Name: OpComplianceSet, Summary: "Change the compliance monitoring settings as one audited batch. Browser session of an administrator with a fresh sign-in only: an API key, an act-as session and a Worker are refused.",
		Scope: ScopeHub, Actions: manage, Mutates: true, SessionOnly: true,
		Args: reflect.TypeFor[ComplianceSetArgs](), Result: reflect.TypeFor[ComplianceSetResult](),
	})
	Register(Op{
		Name: OpComplianceStatus, Summary: "Show the compliance monitoring policy in force (version, level, input, traffic payload, destination) and the state of its export.",
		Scope: ScopeHub, Actions: manage,
		Args: reflect.TypeFor[ComplianceStatusArgs](), Result: reflect.TypeFor[ComplianceStatusResult](),
	})
}
