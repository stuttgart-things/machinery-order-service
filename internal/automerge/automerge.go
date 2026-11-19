// Package automerge decides whether an order's pull request may be merged
// by the service (docs/adr/0002-auto-merge.md): the order asks for it and a
// rule of its ManagementCluster allows it.
package automerge

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	v1alpha1 "github.com/stuttgart-things/machinery-order-service/api/v1alpha1"
)

// DefaultMethod is the merge method when the cluster names none.
const DefaultMethod = "merge"

// Method is the cluster's merge method.
func Method(am *v1alpha1.AutoMerge) string {
	if am == nil || am.Method == "" {
		return DefaultMethod
	}
	return am.Method
}

// Allows reports whether rule r lets requestedBy (with its groups) order
// with auto-merge. A rule without users and groups allows nobody.
func Allows(r v1alpha1.AutoMergeRule, requestedBy string, groups []string) bool {
	if requestedBy != "" && slices.Contains(r.Users, requestedBy) {
		return true
	}
	for _, g := range groups {
		if slices.Contains(r.Groups, g) {
			return true
		}
	}
	return false
}

// Covers reports whether rule r covers the template and the rendered
// parameters; when not, why.
func Covers(r v1alpha1.AutoMergeRule, template string, params map[string]any) (bool, string) {
	if len(r.Templates) > 0 && !slices.Contains(r.Templates, template) {
		return false, fmt.Sprintf("template %s not in %v", template, r.Templates)
	}
	keys := make([]string, 0, len(r.Parameters))
	for k := range r.Parameters {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		v, ok := params[k]
		if !ok {
			return false, fmt.Sprintf("parameter %s is not set", k)
		}
		s := scalar(v)
		if !slices.Contains(r.Parameters[k], s) {
			return false, fmt.Sprintf("%s=%s not in %v", k, s, r.Parameters[k])
		}
	}
	return true, ""
}

// Find returns the first rule of am that allows the order, or nil and why
// none does.
func Find(am *v1alpha1.AutoMerge, requestedBy string, groups []string, template string, params map[string]any) (*v1alpha1.AutoMergeRule, string) {
	if am == nil || len(am.Rules) == 0 {
		return nil, "the management cluster has no auto-merge rules"
	}
	var why []string
	for i := range am.Rules {
		r := am.Rules[i]
		if !Allows(r, requestedBy, groups) {
			why = append(why, fmt.Sprintf("%s: not for %s", r.Name, orNobody(requestedBy)))
			continue
		}
		if ok, reason := Covers(r, template, params); !ok {
			why = append(why, r.Name+": "+reason)
			continue
		}
		return &r, ""
	}
	return nil, strings.Join(why, "; ")
}

// Rule returns the rule named name, or nil.
func Rule(am *v1alpha1.AutoMerge, name string) *v1alpha1.AutoMergeRule {
	if am == nil {
		return nil
	}
	for i := range am.Rules {
		if am.Rules[i].Name == name {
			return &am.Rules[i]
		}
	}
	return nil
}

// Params decodes rendered parameters.
func Params(rd *v1alpha1.Rendered) map[string]any {
	out := map[string]any{}
	if rd != nil && rd.Parameters != nil && len(rd.Parameters.Raw) > 0 {
		_ = json.Unmarshal(rd.Parameters.Raw, &out)
	}
	return out
}

func orNobody(s string) string {
	if s == "" {
		return "an anonymous orderer"
	}
	return s
}

// scalar renders a parameter value as the rule lists it.
func scalar(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case nil:
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}
