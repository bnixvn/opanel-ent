package auth

import "github.com/bnixvn/opanel-ent/internal/db"

// ScopeFor turns a caller into the set of accounts they may see.
//
// One function, used by every listing. The alternative -- each handler
// deciding for itself -- is how a reseller ends up seeing another reseller's
// customers because one endpoint was written before the rule changed.
func ScopeFor(u *db.User) db.Scope {
	switch Role(u.Role) {
	case RoleAdmin:
		return db.ScopeAll()
	case RoleReseller:
		return db.ScopeSubtree(u.ID)
	default:
		return db.ScopeSelf(u.ID)
	}
}

// ParentOf returns a user's reseller id, or zero.
func ParentOf(u *db.User) int64 {
	if u == nil || u.ParentID == nil {
		return 0
	}
	return *u.ParentID
}

// CanSee reports whether actor may read target's account.
func CanSee(actor, target *db.User) bool {
	return ScopeFor(actor).Covers(target.ID, ParentOf(target))
}

// CanManage reports whether actor may change target's account.
//
// Stricter than CanSee in one way that matters: an administrator may not be
// managed by anyone but another administrator, so a reseller who is somehow
// handed an administrator's id cannot suspend them.
func CanManage(actor, target *db.User) bool {
	if actor.ID == target.ID {
		return false // changing your own role or suspension goes through Account
	}
	switch Role(actor.Role) {
	case RoleAdmin:
		return true
	case RoleReseller:
		// Only their own customers, and never another reseller.
		return Role(target.Role) == RoleEndUser && ParentOf(target) == actor.ID
	default:
		return false
	}
}

// CanOwn reports whether actor may create resources on behalf of ownerID.
func CanOwn(actor *db.User, owner *db.User) bool {
	if actor.ID == owner.ID {
		return true
	}
	return CanManage(actor, owner)
}

// OwnsResource reports whether actor may act on something owned by ownerID.
// parentOf is the owner's reseller, zero when they have none.
func OwnsResource(actor *db.User, ownerID, parentOf int64) bool {
	return ScopeFor(actor).Covers(ownerID, parentOf)
}
