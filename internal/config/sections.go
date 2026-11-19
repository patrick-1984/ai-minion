package config

// Sections of the Hub settings page. Every Hub setting belongs to exactly one;
// a test keeps the table complete, so a new setting cannot land in "Other" by
// accident.
const (
	SectionServer    = "Server and network"
	SectionAccounts  = "Sign-in and accounts"
	SectionOIDC      = "Single sign-on (OIDC)"
	SectionWorkers   = "Workers and invitations"
	SectionPipe      = "Messages between sessions"
	SectionUpdates   = "Updates"
	SectionLogging   = "Logging and troubleshooting"
	SectionLogExport = "Log export"
	SectionStorage   = "Storage"
	SectionOther     = "Other"
)

// SectionOrder is the order of the sections on the page.
var SectionOrder = []string{
	SectionServer, SectionAccounts, SectionOIDC, SectionWorkers, SectionPipe,
	SectionUpdates, SectionLogging, SectionLogExport, SectionStorage, SectionOther,
}

// sectionOf maps a Hub setting key to its section and its rank inside the
// section: rank 0 keys come first (the ones a section needs to work at all),
// then the rest in alphabetical order.
var sectionOf = map[string]struct {
	section string
	rank    int
}{
	"listen":            {SectionServer, 1},
	"management_listen": {SectionServer, 1},
	"public_url":        {SectionServer, 1},
	"tls":               {SectionServer, 1},
	"trusted_proxies":   {SectionServer, 1},

	"admin_username": {SectionAccounts, 0},
	"admin_password": {SectionAccounts, 0},
	"session_ttl":    {SectionAccounts, 1},

	"oidc_issuer":                   {SectionOIDC, 0},
	"oidc_client_id":                {SectionOIDC, 0},
	"oidc_client_secret":            {SectionOIDC, 0},
	"oidc_admin_values":             {SectionOIDC, 1},
	"oidc_name":                     {SectionOIDC, 1},
	"oidc_refresh_interval":         {SectionOIDC, 1},
	"oidc_role_claim":               {SectionOIDC, 1},
	"oidc_scopes":                   {SectionOIDC, 1},
	"oidc_timeout":                  {SectionOIDC, 1},
	"oidc_tls_insecure_skip_verify": {SectionOIDC, 1},
	"oidc_user_values":              {SectionOIDC, 1},

	"hidden_sessions":       {SectionWorkers, 1},
	"infra_default_owner":   {SectionWorkers, 1},
	"infra_invite_key_days": {SectionWorkers, 1},

	"pipe_budget_per_day": {SectionPipe, 1},

	"update_channel": {SectionUpdates, 0},
	"update_check":   {SectionUpdates, 1},
	"self_update":    {SectionUpdates, 1},

	"log_level":  {SectionLogging, 1},
	"log_format": {SectionLogging, 1},
	"verbose":    {SectionLogging, 1},

	"syslog":                  {SectionLogExport, 0},
	"syslog_endpoint":         {SectionLogExport, 0},
	"syslog_level":            {SectionLogExport, 0},
	"syslog_max_bytes":        {SectionLogExport, 1},
	"syslog_ca_file":          {SectionLogExport, 1},
	"syslog_client_cert_file": {SectionLogExport, 1},
	"syslog_client_key_file":  {SectionLogExport, 1},
	"otel":                    {SectionLogExport, 2},
	"otel_endpoint":           {SectionLogExport, 2},
	"otel_level":              {SectionLogExport, 2},
	"otel_headers":            {SectionLogExport, 2},
	"otel_ca_file":            {SectionLogExport, 3},
	"otel_client_cert_file":   {SectionLogExport, 3},
	"otel_client_key_file":    {SectionLogExport, 3},

	"worker_traffic_log":         {SectionLogExport, 4},
	"worker_traffic_log_to":      {SectionLogExport, 4},
	"worker_traffic_log_workers": {SectionLogExport, 4},
	"otel_metrics":               {SectionLogExport, 2},

	"data_dir":  {SectionStorage, 1},
	"state_dir": {SectionStorage, 1},
}

// SectionOf returns the settings-page section of a Hub setting and its rank
// inside the section (lower first). An unknown key is in SectionOther.
func SectionOf(key string) (section string, rank int) {
	if s, ok := sectionOf[key]; ok {
		return s.section, s.rank
	}
	return SectionOther, 1
}
