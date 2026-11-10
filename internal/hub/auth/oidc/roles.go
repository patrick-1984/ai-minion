package oidc

import "slices"

// Role names, as stored in user_roles.
const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

// SourceIDP is the source of the role rows that this package writes: IdP sync
// replaces only the rows with this source and never touches local grants
// (AD-10).
const SourceIDP = "idp"

// Rules maps the values of one ID token claim to roles. A value that is in
// Admin gives the admin role, a value in User gives the user role; both can
// apply. The comparison is exact (case-sensitive), and an empty configured
// value matches nothing.
type Rules struct {
	// Claim is the name of the one claim that is read. Empty means no rules.
	Claim string
	Admin []string
	User  []string
}

// Evaluate returns the roles that claims earn, sorted. It reads only the
// configured claim. A missing, empty or malformed claim yields no roles, and
// no matching rule yields no roles; the caller treats an empty result as "no
// access" (AD-10).
//
// The claim may be a string (one value) or a list of strings; elements that
// are not strings are ignored.
func (r Rules) Evaluate(claims map[string]any) []string {
	if r.Claim == "" {
		return nil
	}
	values := claimValues(claims[r.Claim])
	if len(values) == 0 {
		return nil
	}
	var roles []string
	if matches(values, r.Admin) {
		roles = append(roles, RoleAdmin)
	}
	if matches(values, r.User) {
		roles = append(roles, RoleUser)
	}
	slices.Sort(roles)
	return roles
}

// claimValues returns the string values of a claim, without empty strings.
func claimValues(v any) []string {
	switch c := v.(type) {
	case string:
		if c == "" {
			return nil
		}
		return []string{c}
	case []any:
		var out []string
		for _, e := range c {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return slices.DeleteFunc(slices.Clone(c), func(s string) bool { return s == "" })
	}
	return nil
}

func matches(have, want []string) bool {
	for _, w := range want {
		if w != "" && slices.Contains(have, w) {
			return true
		}
	}
	return false
}

// HasAdmin reports whether roles contains the admin role.
func HasAdmin(roles []string) bool { return slices.Contains(roles, RoleAdmin) }
