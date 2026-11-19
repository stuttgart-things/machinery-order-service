package automerge

import (
	"strings"
	"testing"

	v1alpha1 "github.com/stuttgart-things/machinery-order-service/api/v1alpha1"
)

func TestFind(t *testing.T) {
	am := &v1alpha1.AutoMerge{Rules: []v1alpha1.AutoMergeRule{
		{Name: "nobody", Templates: []string{"vm"}},
		{Name: "personas", Groups: []string{"group:default/ai-personas"}, Templates: []string{"vm"},
			Parameters: map[string][]string{"size": {"small"}, "cpu": {"2"}}},
		{Name: "alice", Users: []string{"user:default/alice"}},
	}}
	small := map[string]any{"size": "small", "cpu": float64(2)}
	cases := []struct {
		name, by, tmpl string
		groups         []string
		params         map[string]any
		want, why      string
	}{
		{"group small", "user:default/persona-developer", "vm", []string{"group:default/ai-personas"}, small, "personas", ""},
		{"group medium", "user:default/persona-developer", "vm", []string{"group:default/ai-personas"}, map[string]any{"size": "medium", "cpu": float64(2)}, "", "size=medium not in [small]"},
		{"group missing param", "user:default/p", "vm", []string{"group:default/ai-personas"}, map[string]any{"size": "small"}, "", "parameter cpu is not set"},
		{"other template", "user:default/p", "db", []string{"group:default/ai-personas"}, small, "", "template db not in [vm]"},
		{"user any template", "user:default/alice", "db", nil, nil, "alice", ""},
		{"stranger", "user:default/bob", "vm", []string{"group:default/dev"}, small, "", "nobody: not for user:default/bob"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, why := Find(am, c.by, c.groups, c.tmpl, c.params)
			got := ""
			if r != nil {
				got = r.Name
			}
			if got != c.want {
				t.Fatalf("rule %q, want %q (why: %s)", got, c.want, why)
			}
			if c.why != "" && !strings.Contains(why, c.why) {
				t.Fatalf("why %q, want it to contain %q", why, c.why)
			}
		})
	}
	if r, why := Find(nil, "user:default/alice", nil, "vm", nil); r != nil || !strings.Contains(why, "no auto-merge rules") {
		t.Fatalf("no rules: %v %q", r, why)
	}
	if Method(nil) != "merge" || Method(&v1alpha1.AutoMerge{Method: "squash"}) != "squash" {
		t.Fatal("method")
	}
}
