package auth

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUserRef(t *testing.T) {
	for name, want := range map[string]string{
		"patrick@sthings.example": "user:default/patrick",
		"claims-ci":               "user:default/claims-ci",
		"p.hermann@labda":         "user:default/p.hermann",
	} {
		got, err := Identity{Name: name}.UserRef()
		require.NoError(t, err, name)
		assert.Equal(t, want, got, name)
	}
	for _, bad := range []string{"", "-x", "a b", "@sthings.example", "x/y"} {
		_, err := Identity{Name: bad}.UserRef()
		assert.Error(t, err, bad)
	}
}

func TestGroups(t *testing.T) {
	id := Identity{Roles: []string{"backstage-platform", "backstage-team-sre", "backstage-beta-vm", "portal-admin", "backstage-team-"}}
	assert.Equal(t, []string{"group:default/beta-vm", "group:default/platform-team", "group:default/sre"}, Config{}.Groups(id),
		"sthings-backstage's defaults and prefixes; unknown roles and empty names give nothing")

	custom := Config{RoleGroups: map[string]string{"portal-admin": "group:default/admins"}}
	assert.Equal(t, []string{"group:default/admins", "group:default/beta-vm", "group:default/sre"}, custom.Groups(id),
		"AUTH_ROLE_GROUPS replaces the explicit defaults, the prefixes stay")
}

func TestParseRoleGroups(t *testing.T) {
	m, err := ParseRoleGroups(" a = group:default/x , b=group:team/y ")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"a": "group:default/x", "b": "group:team/y"}, m)

	m, err = ParseRoleGroups("")
	require.NoError(t, err)
	assert.Nil(t, m)

	for _, bad := range []string{"a", "=group:default/x", "a=user:default/x"} {
		_, err := ParseRoleGroups(bad)
		assert.Error(t, err, bad)
	}
}

func TestMayOrder(t *testing.T) {
	c := Config{AdminRoles: []string{"backstage-platform"}}
	dev := Identity{Roles: []string{"backstage-developer"}}
	admin := Identity{Roles: []string{"backstage-platform"}}
	assert.True(t, c.MayOrder(dev, nil), "no limit")
	assert.True(t, c.MayOrder(dev, []string{"backstage-developer"}))
	assert.False(t, c.MayOrder(dev, []string{"backstage-sre"}))
	assert.True(t, c.MayOrder(admin, []string{"backstage-sre"}), "admins may order everything")
}
