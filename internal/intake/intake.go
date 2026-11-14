// Package intake turns an Order into rendered files and a pull request: the
// parameters the placement adds, the render through claim-machinery-api, the
// XR reference, and the branch/title/body conventions of the claims CLI
// (stuttgart-things/claims#92), so the existing checks treat the PR like a
// create-vm PR.
package intake

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/stuttgart-things/machinery-order-service/api/v1alpha1"
	"github.com/stuttgart-things/machinery-order-service/internal/layout"
	"github.com/stuttgart-things/machinery-order-service/internal/render"
)

// Parameters the service sets from the order and its placement; an orderer
// may not set them.
const (
	ParamOrderID           = "order_id"
	ParamManagementCluster = "management_cluster"
	ParamSourcePath        = "source_path"
	ParamEnvironmentConfig = "environment_config"
)

// dnsLabel is a name that is a file name, a branch suffix and part of an
// object name at once.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,48}[a-z0-9])?$`)

// Reserved are the parameters the service owns.
var Reserved = []string{ParamOrderID, ParamManagementCluster, ParamSourcePath, ParamEnvironmentConfig}

// NewOrderID returns a fresh order ID, ord-<12 hex>. It is the Order's name.
func NewOrderID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "ord-" + hex.EncodeToString(b)
}

// Renderer is what intake needs from claim-machinery-api. *render.Client
// implements it.
type Renderer interface {
	Render(ctx context.Context, template string, params map[string]any, callerToken string) (string, error)
	Template(ctx context.Context, name, callerToken string) (*render.Template, error)
}

// InvalidError is an order that cannot be rendered as given (the orderer's
// fault).
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

// UserParameters decodes spec.parameters.
func UserParameters(o *v1alpha1.Order) (map[string]any, error) {
	return decode(o.Spec.Parameters)
}

func decode(raw *runtime.RawExtension) (map[string]any, error) {
	out := map[string]any{}
	if raw == nil || len(raw.Raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(raw.Raw, &out); err != nil {
		return nil, &InvalidError{Msg: fmt.Sprintf("parameters must be a JSON object: %v", err)}
	}
	return out, nil
}

// Encode turns parameters into a RawExtension.
func Encode(params map[string]any) (*runtime.RawExtension, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	return &runtime.RawExtension{Raw: raw}, nil
}

// Parameters are the orderer's parameters plus what the order and its
// management cluster add: order_id, management_cluster, and, when the
// cluster names them, source_path and environment_config.
func Parameters(o *v1alpha1.Order, mc *v1alpha1.ManagementCluster) (map[string]any, error) {
	params, err := UserParameters(o)
	if err != nil {
		return nil, err
	}
	for _, k := range Reserved {
		if _, ok := params[k]; ok {
			return nil, &InvalidError{Msg: fmt.Sprintf("parameter %s is set by the service; leave it out", k)}
		}
	}
	params[ParamOrderID] = o.Name
	cluster := mc.Spec.Render.ClusterName
	if cluster == "" {
		cluster = mc.Name
	}
	params[ParamManagementCluster] = cluster
	if dir := strings.TrimRight(mc.Spec.GitOps.OrdersPath, "/"); dir != "" {
		name, _ := params["name"].(string)
		if name == "" {
			return nil, &InvalidError{Msg: "parameter name is required: it names the order's file"}
		}
		if !dnsLabel.MatchString(name) {
			return nil, &InvalidError{Msg: fmt.Sprintf("name %q: want lower-case letters, digits and '-', at most 50 characters", name)}
		}
		p := path.Join(dir, name+".yaml")
		if err := layout.ValidatePath(p); err != nil {
			return nil, &InvalidError{Msg: fmt.Sprintf("name %q: %v", name, err)}
		}
		params[ParamSourcePath] = p
	}
	if ec := mc.Spec.Render.EnvironmentConfig; ec != "" {
		params[ParamEnvironmentConfig] = ec
	}
	return params, nil
}

// Render renders the order for its management cluster. callerToken is the
// orderer's bearer token ("" = the service's own).
func Render(ctx context.Context, r Renderer, o *v1alpha1.Order, mc *v1alpha1.ManagementCluster, callerToken string) (*v1alpha1.Rendered, error) {
	params, err := Parameters(o, mc)
	if err != nil {
		return nil, err
	}
	out, err := r.Render(ctx, o.Spec.Template, params, callerToken)
	if err != nil {
		return nil, err
	}
	files, err := layout.Plan(out)
	if err != nil {
		return nil, &InvalidError{Msg: fmt.Sprintf("template %s: %v", o.Spec.Template, err)}
	}
	if _, ok := layout.XR(files); !ok {
		return nil, &InvalidError{Msg: fmt.Sprintf("template %s: no document names its own file (%s)", o.Spec.Template, layout.AnnotationSourcePath)}
	}
	raw, err := Encode(params)
	if err != nil {
		return nil, err
	}
	rd := &v1alpha1.Rendered{Parameters: raw, RenderedAt: metav1.NewTime(time.Now().UTC().Truncate(time.Second))}
	for _, f := range files {
		rd.Files = append(rd.Files, v1alpha1.RenderedFile{Path: f.Path, Content: f.Content()})
	}
	return rd, nil
}

// Files re-reads the rendered files into documents.
func Files(rd *v1alpha1.Rendered) ([]layout.File, error) {
	var out []layout.File
	for _, f := range rd.Files {
		docs, err := layout.SplitDocuments(f.Content)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Path, err)
		}
		out = append(out, layout.File{Path: f.Path, Documents: docs})
	}
	return out, nil
}

// XRRef names the order's XR.
func XRRef(rd *v1alpha1.Rendered) (*v1alpha1.ObjectRef, error) {
	files, err := Files(rd)
	if err != nil {
		return nil, err
	}
	xr, ok := layout.XR(files)
	if !ok {
		return nil, fmt.Errorf("no document names its own file (%s)", layout.AnnotationSourcePath)
	}
	return &v1alpha1.ObjectRef{APIVersion: xr.APIVersion, Kind: xr.Kind, Name: xr.Name, Namespace: xr.Namespace}, nil
}

// Paths are the files' paths.
func Paths(rd *v1alpha1.Rendered) []string {
	out := make([]string, 0, len(rd.Files))
	for _, f := range rd.Files {
		out = append(out, f.Path)
	}
	return out
}

// Convention is the branch, title and body of an order's pull request.
type Convention struct {
	Branch string
	Title  string
	Body   string
}

// ConventionFor follows the claims CLI (cmd/render_layout.go conventionFor),
// which follows the Backstage templates: a VM order like create-vm's (branch
// vm-<name>, "feat(machinery): VM <name> (<use case>, <lab>) (MERGE = BAU)"),
// anything else as claim-<template>-<name>. tmpl may be nil.
func ConventionFor(o *v1alpha1.Order, rd *v1alpha1.Rendered, tmpl *render.Template) (Convention, error) {
	params, err := decode(rd.Parameters)
	if err != nil {
		return Convention{}, err
	}
	param := func(names ...string) string {
		for _, n := range names {
			if v, ok := params[n]; ok && fmt.Sprintf("%v", v) != "" {
				return fmt.Sprintf("%v", v)
			}
		}
		if tmpl != nil {
			for _, n := range names {
				if v, ok := tmpl.Defaults[n]; ok && fmt.Sprintf("%v", v) != "" {
					return fmt.Sprintf("%v", v)
				}
			}
		}
		return ""
	}
	files, err := Files(rd)
	if err != nil {
		return Convention{}, err
	}
	xr, _ := layout.XR(files)
	name := param("name")
	if name == "" {
		name = strings.TrimSuffix(xr.Name, "-vm")
	}

	var list strings.Builder
	for _, f := range rd.Files {
		_, _ = fmt.Fprintf(&list, "- `%s`\n", f.Path)
	}
	footer := fmt.Sprintf("\nOrder ID: `%s`", o.Name)
	if o.Spec.RequestedBy != "" {
		footer += fmt.Sprintf(" · requested by `%s`", o.Spec.RequestedBy)
	}
	if o.Spec.Reason != "" {
		footer += "\n\nReason: " + o.Spec.Reason
	}
	footer += "\n\n---\n*Generated by machinery-order-service*\n"

	if tmpl != nil && tmpl.Type == "xr-native-vm" {
		lab := param("templateName", "lab")
		useCase := param("use_case")
		size := param("size")
		cluster := xr.Annotations[layout.AnnotationManagementCluster]
		ordersDir := path.Dir(xr.Annotations[layout.AnnotationSourcePath])

		var b strings.Builder
		_, _ = fmt.Fprintf(&b, "Order for the VM `%s`, written by machinery-order-service from the claim-machinery-api template `%s`.\n\n", name, o.Spec.Template)
		b.WriteString("| Decision | Value |\n|---|---|\n")
		_, _ = fmt.Fprintf(&b, "| Lab | `%s` |\n| Size | `%s` |\n| Use case | `%s` |\n\n", lab, size, useCase)
		_, _ = fmt.Fprintf(&b, "**Merging builds the VM.** Flux applies the order on `%s`, Crossplane creates the VM, "+
			"and its AnsibleRun sets it up once it has an address. The address is in the XR's status "+
			"(`kubectl get nativeproxmoxvm,nativevspherevm %s` on `%s`) and `ssh sthings@IP` works with the address from there.\n\n",
			cluster, xr.Name, cluster)
		_, _ = fmt.Fprintf(&b, "This Kustomization does not prune, and the order carries `prune: disabled`. "+
			"Removing the VM is a deliberate two-step, see `%s/README.md`.\n\n", ordersDir)
		b.WriteString("Files:\n\n")
		b.WriteString(list.String())
		b.WriteString(footer)
		return Convention{
			Branch: "vm-" + name,
			Title:  fmt.Sprintf("feat(machinery): VM %s (%s, %s) (MERGE = BAU)", name, useCase, lab),
			Body:   b.String(),
		}, nil
	}

	var b strings.Builder
	_, _ = fmt.Fprintf(&b, "Rendered by machinery-order-service from the claim-machinery-api template `%s`.\n\nFiles:\n\n", o.Spec.Template)
	b.WriteString(list.String())
	b.WriteString(footer)
	return Convention{
		Branch: fmt.Sprintf("claim-%s-%s", o.Spec.Template, name),
		Title:  fmt.Sprintf("feat(claims): %s %s", o.Spec.Template, name),
		Body:   b.String(),
	}, nil
}
