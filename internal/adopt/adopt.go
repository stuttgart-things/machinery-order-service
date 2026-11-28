// Package adopt hands a hand-written XR to machinery-order-service as an
// order (#4823, docs/adr/0004-recover-adopt.md). The pull request adds the
// order-id label and annotation and the template annotation to the XR's
// file, nothing else; once it is merged the order follows the XR like any
// order, and it can be torn down through the service.
//
// Refused: a cluster without spec.adoptKinds (adopt off) or a kind not in
// it, a file outside spec.adoptPaths, a cluster's spec.adoptExclude, a kind
// outside spec.allowedKinds,
// an XR that carries an order-id already (recover it instead), a file in the
// orders directory, an XR another Order follows, and a template whose
// rendered XR is of another kind.
package adopt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stuttgart-things/machinery-order-service/api/v1alpha1"
	"github.com/stuttgart-things/machinery-order-service/internal/capabilities"
	"github.com/stuttgart-things/machinery-order-service/internal/intake"
	"github.com/stuttgart-things/machinery-order-service/internal/layout"
	"github.com/stuttgart-things/machinery-order-service/internal/recovery"
	"github.com/stuttgart-things/machinery-order-service/internal/yamledit"
)

// TemplateUnknown prefixes spec.template when the request names none: the
// origin stays visible (recovered:<Kind> is recovery's).
const TemplateUnknown = "adopted:"

// Request is what to adopt.
type Request struct {
	ManagementCluster string `json:"managementCluster"`
	// Path is the XR's file on the base branch.
	Path string `json:"path"`
	// Template is the ClaimTemplate the XR stands for; optional. Its
	// rendered XR must be of the XR's kind (a test render with name = the
	// XR's name plus Parameters).
	Template string `json:"template,omitempty"`
	// Parameters are added to the test render (required template
	// parameters other than name).
	Parameters map[string]any `json:"parameters,omitempty"`
	Reason     string         `json:"reason,omitempty"`
	DryRun     bool           `json:"dryRun,omitempty"`
}

// Result is what adopt did (dry run: would do).
type Result struct {
	OrderID   string   `json:"orderID"`
	Path      string   `json:"path"`
	Kind      string   `json:"kind"`
	Name      string   `json:"name"`
	Namespace string   `json:"namespace,omitempty"`
	Template  string   `json:"template"`
	Added     []string `json:"added"`
	DryRun    bool     `json:"dryRun"`

	Order *v1alpha1.Order `json:"-"`
}

// Error is a refused adopt: Status is the HTTP status to answer.
type Error struct {
	Status int
	Msg    string
}

func (e *Error) Error() string { return e.Msg }

func refuse(status int, format string, args ...any) error {
	return &Error{Status: status, Msg: fmt.Sprintf(format, args...)}
}

// Plan checks the request and builds the Order (not created). by is the
// caller (user:default/<name>), token its bearer token for the test render.
func Plan(ctx context.Context, c client.Client, git recovery.Git, r intake.Renderer, mc *v1alpha1.ManagementCluster,
	namespace string, req Request, by, token string) (*Result, error) {
	if len(mc.Spec.AdoptKinds) == 0 {
		return nil, refuse(403, "adopt is off on ManagementCluster %s (spec.adoptKinds is empty)", mc.Name)
	}
	if err := layout.ValidatePath(req.Path); err != nil {
		return nil, refuse(400, "path: %v", err)
	}
	if len(req.Reason) > 500 {
		return nil, refuse(400, "reason: at most 500 characters")
	}
	if dir := strings.TrimRight(mc.Spec.GitOps.OrdersPath, "/"); dir != "" && path.Dir(req.Path) == dir {
		return nil, refuse(400, "%s is in the orders directory: those are orders already (POST /api/v1/recover)", req.Path)
	}
	if !slices.Contains(recovery.CleanDirs(mc.Spec.AdoptPaths), path.Dir(req.Path)) {
		return nil, refuse(400, "%s is not in a directory of spec.adoptPaths of ManagementCluster %s (%s): recovery would not find the order",
			req.Path, mc.Name, strings.Join(mc.Spec.AdoptPaths, ", "))
	}
	repo, base := recovery.Repository(mc)
	content, ok, err := git.File(ctx, repo, base, req.Path)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, refuse(400, "%s is not on %s", req.Path, base)
	}
	docs, err := layout.SplitDocuments(content)
	if err != nil {
		return nil, refuse(400, "%s: %v", req.Path, err)
	}
	var xr *layout.Document
	idx := -1
	for i := range docs {
		if docs[i].Annotations[layout.AnnotationSourcePath] == req.Path {
			if xr != nil {
				return nil, refuse(400, "%s holds more than one document that names it (%s): adopt takes one XR per file", req.Path, layout.AnnotationSourcePath)
			}
			xr, idx = &docs[i], i
		}
	}
	if xr == nil {
		return nil, refuse(400, "%s has no document that names its own file (%s): not a hand-written XR adopt can take", req.Path, layout.AnnotationSourcePath)
	}
	if id := xr.Annotations[v1alpha1.OrderIDKey]; id != "" {
		return nil, refuse(409, "%s %s is order %s already (POST /api/v1/recover if its Order is gone)", xr.Kind, xr.Name, id)
	}
	if id := xr.Labels[v1alpha1.OrderIDKey]; id != "" {
		return nil, refuse(409, "%s %s carries the order-id label %s already", xr.Kind, xr.Name, id)
	}
	if slices.Contains(mc.Spec.AdoptExclude, xr.Name) {
		return nil, refuse(403, "%s is in spec.adoptExclude of ManagementCluster %s: it is not to be adopted", xr.Name, mc.Name)
	}
	cluster := mc.Spec.Render.ClusterName
	if cluster == "" {
		cluster = mc.Name
	}
	if mcl := xr.Annotations[layout.AnnotationManagementCluster]; mcl != "" && mcl != cluster {
		return nil, refuse(400, "%s names management cluster %s, not %s", req.Path, mcl, cluster)
	}
	x := capabilities.XR{APIVersion: xr.APIVersion, Kind: xr.Kind}
	if !capabilities.Listed(mc.Spec.AdoptKinds, x) {
		return nil, refuse(403, "ManagementCluster %s adopts no %s (adoptKinds: %s)", mc.Name, capabilities.Name(x), strings.Join(mc.Spec.AdoptKinds, ", "))
	}
	if why := capabilities.Allowed(mc, x); why != "" {
		return nil, refuse(400, "%s", why)
	}
	// another Order (an open adopt, say) has this file already
	var orders v1alpha1.OrderList
	if err := c.List(ctx, &orders, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	for _, o := range orders.Items {
		if o.Status.Phase.Terminal() || o.TornDown() {
			continue
		}
		if a := o.Spec.Adopted; a != nil && a.Path == req.Path {
			return nil, refuse(409, "order %s adopts %s already (%s)", o.Name, req.Path, o.Status.Phase)
		}
	}

	template := req.Template
	if template == "" {
		template = TemplateUnknown + xr.Kind
	} else if err := checkKind(ctx, r, mc, xr, req, token); err != nil {
		return nil, err
	}

	id := intake.NewOrderID()
	doc := xr.Content
	var added []string
	for _, e := range []struct{ field, key, value string }{
		{"labels", v1alpha1.OrderIDKey, id},
		{"annotations", v1alpha1.OrderIDKey, id},
		{"annotations", v1alpha1.TemplateKey, template},
	} {
		out, changed, err := yamledit.SetMetadata(doc, e.field, e.key, e.value)
		if err != nil {
			return nil, refuse(400, "%s: %v", req.Path, err)
		}
		if changed {
			added = append(added, fmt.Sprintf("metadata.%s.%s: %s", e.field, e.key, e.value))
		}
		doc = out
	}
	edited := replaceDocument(content, docs, idx, doc)

	sum := sha256.Sum256([]byte(content))
	now := metav1.Now()
	o := &v1alpha1.Order{
		ObjectMeta: metav1.ObjectMeta{
			Name: id, Namespace: namespace,
			Labels: map[string]string{v1alpha1.OrderIDKey: id},
		},
		Spec: v1alpha1.OrderSpec{
			Template:    template,
			RequestedBy: by,
			Reason:      req.Reason,
			Placement:   v1alpha1.Placement{ManagementCluster: mc.Name},
			Delivery:    v1alpha1.DeliveryPullRequest,
			Rendered: &v1alpha1.Rendered{
				Files:      []v1alpha1.RenderedFile{{Path: req.Path, Content: edited}},
				RenderedAt: metav1.NewTime(time.Now().UTC().Truncate(time.Second)),
			},
			Adopted: &v1alpha1.AdoptedFrom{Path: req.Path, ContentSHA256: hex.EncodeToString(sum[:]), By: by, At: now},
		},
	}
	if by != "" {
		o.Annotations = map[string]string{v1alpha1.RequestedByKey: by}
	}
	return &Result{OrderID: id, Path: req.Path, Kind: xr.Kind, Name: xr.Name, Namespace: xr.Namespace,
		Template: template, Added: added, DryRun: req.DryRun, Order: o}, nil
}

// replaceDocument puts doc in place of document idx of content, leaving the
// rest of the file (comments between documents included) as it is.
func replaceDocument(content string, docs []layout.Document, idx int, doc string) string {
	old := docs[idx].Content
	i := strings.Index(content, old)
	if i < 0 || strings.Count(content, old) != 1 {
		// cannot happen with SplitDocuments' parts; keep the file whole
		var b strings.Builder
		for j, d := range docs {
			if j == idx {
				d.Content = doc
			}
			b.WriteString("---" + d.Content)
		}
		return b.String()
	}
	return content[:i] + doc + content[i+len(old):]
}

// checkKind renders the template once (name = the XR's name, plus the
// request's parameters) and compares the rendered XR with the hand-written
// one: same kind and API group.
func checkKind(ctx context.Context, r intake.Renderer, mc *v1alpha1.ManagementCluster, xr *layout.Document, req Request, token string) error {
	params := map[string]any{"name": xr.Name}
	for k, v := range req.Parameters {
		params[k] = v
	}
	raw, err := intake.Encode(params)
	if err != nil {
		return refuse(400, "parameters: %v", err)
	}
	probe := &v1alpha1.Order{
		ObjectMeta: metav1.ObjectMeta{Name: "ord-000000000000"},
		Spec:       v1alpha1.OrderSpec{Template: req.Template, Parameters: raw, Placement: v1alpha1.Placement{ManagementCluster: mc.Name}},
	}
	rd, err := intake.Render(ctx, r, probe, mc, token)
	if err != nil {
		return refuse(400, "template %s: cannot check its kind with a test render (name=%s; pass the other required parameters in \"parameters\"): %v",
			req.Template, xr.Name, err)
	}
	ref, err := intake.XRRef(rd)
	if err != nil {
		return refuse(400, "template %s: %v", req.Template, err)
	}
	group := func(apiVersion string) string { g, _, _ := strings.Cut(apiVersion, "/"); return g }
	if ref.Kind != xr.Kind || group(ref.APIVersion) != group(xr.APIVersion) {
		return refuse(400, "template %s renders %s (%s), not %s (%s)", req.Template, ref.Kind, ref.APIVersion, xr.Kind, xr.APIVersion)
	}
	return nil
}
