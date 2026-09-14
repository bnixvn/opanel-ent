package auth

import "slices"

// Role is a panel role. Roles are coarse; fine-grained machine access is
// expressed with Scope on API tokens.
type Role string

const (
	RoleAdmin    Role = "admin"
	RoleReseller Role = "reseller"
	RoleEndUser  Role = "end_user"
)

// AllRoles lists every role, broadest first, for pickers in the interface.
var AllRoles = []Role{RoleAdmin, RoleReseller, RoleEndUser}

// Valid reports whether r is a role the panel knows.
func (r Role) Valid() bool {
	switch r {
	case RoleAdmin, RoleReseller, RoleEndUser:
		return true
	}
	return false
}

// rank orders roles by breadth of access. Higher outranks lower.
func (r Role) rank() int {
	switch r {
	case RoleAdmin:
		return 3
	case RoleReseller:
		return 2
	case RoleEndUser:
		return 1
	}
	return 0
}

// AtLeast reports whether r has at least the access of min.
func (r Role) AtLeast(min Role) bool { return r.rank() >= min.rank() }

// Scope limits what an API token may do. Sessions are not scoped; the role
// governs them.
type Scope string

const (
	ScopeProvisioningManage Scope = "provisioning:manage"
	ScopeSSOCreate          Scope = "sso:create"
	ScopeUsageRead          Scope = "usage:read"
)

// AllScopes lists every scope that may be granted to a token.
var AllScopes = []Scope{ScopeProvisioningManage, ScopeSSOCreate, ScopeUsageRead}

// ValidScope reports whether s is a scope the panel knows.
func ValidScope(s Scope) bool { return slices.Contains(AllScopes, s) }
