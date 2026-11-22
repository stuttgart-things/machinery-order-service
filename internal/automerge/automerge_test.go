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
			r, why := Find(am, v1alpha1.ActionOrder, c.by, c.groups, c.tmpl, c.params)
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
	if r, why := Find(nil, v1alpha1.ActionOrder, "user:default/alice", nil, "vm", nil); r != nil || !strings.Contains(why, "no auto-merge rules") {
		t.Fatalf("no rules: %v %q", r, why)
	}
	if Method(nil) != "merge" || Method(&v1alpha1.AutoMerge{Method: "squash"}) != "squash" {
		t.Fatal("method")
	}
}

func TestFindTeardown(t *testing.T) {
	am := &v1alpha1.AutoMerge{Rules: []v1alpha1.AutoMergeRule{
		{Name: "orders-only", Groups: []string{"group:default/ai-personas"}},
		{Name: "both", Users: []string{"user:default/alice"}, Actions: []v1alpha1.AutoMergeAction{v1alpha1.ActionOrder, v1alpha1.ActionTeardown}},
		{Name: "teardown-only", Users: []string{"user:default/bob"}, Actions: []v1alpha1.AutoMergeAction{v1alpha1.ActionTeardown}},
	}}
	personas := []string{"group:default/ai-personas"}
	if r, why := Find(am, v1alpha1.ActionTeardown, "user:default/p", personas, "vm", nil); r != nil || !strings.Contains(why, "orders-only: not for teardowns") {
		t.Fatalf("a rule without actions allowed a teardown: %v %q", r, why)
	}
	if r, _ := Find(am, v1alpha1.ActionOrder, "user:default/p", personas, "vm", nil); r == nil || r.Name != "orders-only" {
		t.Fatalf("order: %v", r)
	}
	if r, _ := Find(am, v1alpha1.ActionTeardown, "user:default/alice", nil, "vm", nil); r == nil || r.Name != "both" {
		t.Fatalf("teardown alice: %v", r)
	}
	if r, _ := Find(am, v1alpha1.ActionOrder, "user:default/bob", nil, "vm", nil); r != nil {
		t.Fatalf("a teardown-only rule allowed an order: %v", r)
	}
}
