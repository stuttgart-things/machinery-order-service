package auth

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// The orderer as the Backstage catalog knows it (#122). sthings-backstage's
// ZITADEL sign-in resolver (packages/backend/src/plugins/auth-zitadel) makes
// the same refs from the same token, so what the API stamps into an order is
// what Backstage shows as its requester and owner:
//
//   - user: user:default/<preferred_username without the login domain>
//   - groups: the ZITADEL project roles, mapped by DefaultRoleGroups (or
//     AUTH_ROLE_GROUPS) and by name (RoleGroupPrefixes)

// DefaultRoleGroups is sthings-backstage's DEFAULT_ROLE_GROUPS.
var DefaultRoleGroups = map[string]string{
	"backstage-platform":  "group:default/platform-team",
	"backstage-developer": "group:default/guests",
}

// RoleGroupPrefixes is sthings-backstage's ROLE_GROUP_PREFIXES: roles mapped
// by name, checked after the explicit role groups.
var RoleGroupPrefixes = []struct{ Role, Group string }{
	{"backstage-beta-", "group:default/beta-"},
	{"backstage-team-", "group:default/"},
}

// entityName is a Backstage entity name: letters, digits, '-', '_', '.', at
// most 63 characters, starting and ending with a letter or digit.
var entityName = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9._-]{0,61}[a-zA-Z0-9])?$`)

// ParseRoleGroups reads "role=group:default/x,role2=group:default/y"; empty
// gives nil (the defaults).
func ParseRoleGroups(s string) (map[string]string, error) {
	items := SplitList(s)
	if len(items) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(items))
	for _, it := range items {
		role, group, ok := strings.Cut(it, "=")
		role, group = strings.TrimSpace(role), strings.TrimSpace(group)
		if !ok || role == "" || !strings.HasPrefix(group, "group:") {
			return nil, fmt.Errorf("%q: want role=group:<namespace>/<name>", it)
		}
		out[role] = group
	}
	return out, nil
}

// UserRef is the orderer's Backstage user ref, user:default/<name>: the
// identity's name without a login domain (patrick@sthings.example ->
// patrick). An error when that is no valid entity name.
func (id Identity) UserRef() (string, error) {
	name, _, _ := strings.Cut(id.Name, "@")
	if !entityName.MatchString(name) {
		return "", fmt.Errorf("%q is not a valid Backstage user name", id.Name)
	}
	return "user:default/" + name, nil
}

// Groups are the Backstage groups the identity's roles make it a member of,
// sorted.
func (c Config) Groups(id Identity) []string {
	explicit := c.RoleGroups
	if explicit == nil {
		explicit = DefaultRoleGroups
	}
	set := map[string]bool{}
	for _, r := range id.Roles {
		if g, ok := explicit[r]; ok {
			set[g] = true
			continue
		}
		for _, p := range RoleGroupPrefixes {
			if rest, ok := strings.CutPrefix(r, p.Role); ok && entityName.MatchString(rest) {
				set[p.Group+rest] = true
				break
			}
		}
	}
	out := make([]string, 0, len(set))
	for g := range set {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

// IsAdmin reports whether the identity has one of the admin roles.
func (c Config) IsAdmin(id Identity) bool {
	return intersects(id.Roles, c.AdminRoles)
}

// MayOrder reports whether the identity may order a template limited to
// roles: no limit, one of them, or an admin.
func (c Config) MayOrder(id Identity, roles []string) bool {
	return len(roles) == 0 || intersects(id.Roles, roles) || c.IsAdmin(id)
}
