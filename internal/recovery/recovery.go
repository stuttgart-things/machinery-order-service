// Package recovery rebuilds Orders whose CRs are gone from what git still
// holds (#4823, docs/adr/0004-recover-adopt.md): every XR file in a
// ManagementCluster's orders directory that carries an order-id, the commit
// with its Order-Id trailer and that commit's pull request. A recovered
// Order starts in phase Recovered and from there follows the live XR like
// any applied order; it can be torn down like one.
//
// Recovery runs only on request (POST /api/v1/recover, admins): deleting an
// Order CR never tears anything down, so an automatic scan would also bring
// back an Order someone removed on purpose.
package recovery

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/stuttgart-things/machinery-order-service/api/v1alpha1"
	"github.com/stuttgart-things/machinery-order-service/internal/delivery"
	"github.com/stuttgart-things/machinery-order-service/internal/layout"
)

// Git is what recovery reads from the repository. *delivery.GitHub
// implements it.
type Git interface {
	Dir(ctx context.Context, repository, ref, dir string) ([]delivery.RepoFile, error)
	File(ctx context.Context, repository, ref, p string) (string, bool, error)
	Origin(ctx context.Context, repository, ref, p, id string) (*delivery.Origin, error)
}

// TemplateUnknown prefixes spec.template of an Order recovered from a file
// written before the service recorded the template (stuttgart-things.com/template).
const TemplateUnknown = "recovered:"

// orderID is what an Order name must look like to be recovered: a DNS
// label, as the Order's name is one.
var orderID = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Candidate is one order found in git.
type Candidate struct {
	OrderID     string   `json:"orderID"`
	Path        string   `json:"path"`
	Kind        string   `json:"kind"`
	Name        string   `json:"name"`
	Namespace   string   `json:"namespace,omitempty"`
	Template    string   `json:"template"`
	RequestedBy string   `json:"requestedBy,omitempty"`
	Files       []string `json:"files"`
	CommitSHA   string   `json:"commitSHA,omitempty"`
	PullRequest string   `json:"pullRequest,omitempty"`
	// Skipped says why the order is not recovered.
	Skipped string `json:"skipped,omitempty"`

	order *v1alpha1.Order
}

// Result is one recovery run on one ManagementCluster.
type Result struct {
	ManagementCluster string `json:"managementCluster"`
	DryRun            bool   `json:"dryRun"`
	// Existing counts the order files whose Order is there.
	Existing int `json:"existing"`
	// Recovered are the Orders created (dry run: that would be).
	Recovered []Candidate `json:"recovered"`
	// Skipped are order files that are not recovered, with the reason.
	Skipped []Candidate `json:"skipped,omitempty"`
}

// Repository and BaseBranch of a ManagementCluster, with the defaults the
// controller uses.
func Repository(mc *v1alpha1.ManagementCluster) (string, string) {
	repo, base := mc.Spec.GitOps.Repository, mc.Spec.GitOps.BaseBranch
	if repo == "" {
		repo = "stuttgart-things/stuttgart-things"
	}
	if base == "" {
		base = "main"
	}
	return repo, base
}

// Run recovers the Orders of mc that are missing in namespace. by is who
// asked; with dryRun nothing is created.
func Run(ctx context.Context, c client.Client, git Git, mc *v1alpha1.ManagementCluster, namespace, by string, dryRun bool) (*Result, error) {
	dir := strings.TrimRight(mc.Spec.GitOps.OrdersPath, "/")
	if dir == "" {
		return nil, fmt.Errorf("ManagementCluster %s has no spec.gitops.ordersPath", mc.Name)
	}
	repo, base := Repository(mc)
	files, err := git.Dir(ctx, repo, base, dir)
	if err != nil {
		return nil, err
	}
	res := &Result{ManagementCluster: mc.Name, DryRun: dryRun, Recovered: []Candidate{}}
	cluster := mc.Spec.Render.ClusterName
	if cluster == "" {
		cluster = mc.Name
	}
	seen := map[string]string{}
	for _, f := range files {
		docs, err := layout.SplitDocuments(f.Content)
		if err != nil {
			res.Skipped = append(res.Skipped, Candidate{Path: f.Path, Skipped: err.Error()})
			continue
		}
		for _, d := range docs {
			id := d.Annotations[v1alpha1.OrderIDKey]
			if id == "" || d.Annotations[layout.AnnotationSourcePath] != f.Path {
				continue // not an order's XR, or a hand-written one
			}
			cand := Candidate{OrderID: id, Path: f.Path, Kind: d.Kind, Name: d.Name, Namespace: d.Namespace,
				Template: d.Annotations[v1alpha1.TemplateKey], RequestedBy: d.Annotations[v1alpha1.RequestedByKey]}
			if cand.Template == "" {
				cand.Template = TemplateUnknown + d.Kind
			}
			skip := func(why string) { cand.Skipped = why; res.Skipped = append(res.Skipped, cand) }
			switch other, dup := seen[id]; {
			case !orderID.MatchString(id):
				skip(fmt.Sprintf("order-id %q is not a valid Order name", id))
				continue
			case dup:
				skip("the order-id is also in " + other)
				continue
			}
			seen[id] = f.Path
			if mcl := d.Annotations[layout.AnnotationManagementCluster]; mcl != "" && mcl != cluster {
				skip(fmt.Sprintf("the file names management cluster %s, not %s", mcl, cluster))
				continue
			}
			err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: id}, &v1alpha1.Order{})
			if err == nil {
				res.Existing++
				continue
			}
			if !apierrors.IsNotFound(err) {
				return nil, err
			}
			if err := build(ctx, git, mc, repo, base, dir, files, &cand, namespace, by); err != nil {
				return nil, err
			}
			res.Recovered = append(res.Recovered, cand)
		}
	}
	if dryRun {
		return res, nil
	}
	for i := range res.Recovered {
		cand := &res.Recovered[i]
		if err := c.Create(ctx, cand.order); err != nil {
			if apierrors.IsAlreadyExists(err) {
				cand.Skipped = "an Order of that ID appeared meanwhile"
				continue
			}
			return res, fmt.Errorf("creating Order %s: %w", cand.OrderID, err)
		}
	}
	return res, nil
}

// build finds the order's commit, pull request and files and makes the
// Order.
func build(ctx context.Context, git Git, mc *v1alpha1.ManagementCluster, repo, base, dir string, dirFiles []delivery.RepoFile,
	cand *Candidate, namespace, by string) error {
	origin, err := git.Origin(ctx, repo, base, cand.Path, cand.OrderID)
	if err != nil {
		return err
	}
	content := map[string]string{}
	for _, f := range dirFiles {
		content[f.Path] = f.Content
	}
	// The order's files: those its commit brought in (an order may write
	// outside its directory, target-path), else the ones in the directory;
	// either way only files whose documents carry its order-id.
	paths := []string{}
	if origin != nil {
		paths = append(paths, origin.Files...)
	} else {
		for _, f := range dirFiles {
			paths = append(paths, f.Path)
		}
	}
	sort.Strings(paths)
	rd := &v1alpha1.Rendered{RenderedAt: metav1.NewTime(time.Now().UTC().Truncate(time.Second))}
	var rest []v1alpha1.RenderedFile
	for _, p := range paths {
		text, ok := content[p]
		if !ok && path.Dir(p) != dir {
			if text, ok, err = git.File(ctx, repo, base, p); err != nil {
				return err
			}
		}
		if !ok || !carries(text, cand.OrderID) {
			continue
		}
		f := v1alpha1.RenderedFile{Path: p, Content: text}
		if p == cand.Path {
			rd.Files = append(rd.Files, f)
		} else {
			rest = append(rest, f)
		}
	}
	if len(rd.Files) == 0 {
		// the XR file itself, even when the commit did not list it
		rd.Files = append(rd.Files, v1alpha1.RenderedFile{Path: cand.Path, Content: content[cand.Path]})
	}
	rd.Files = append(rd.Files, rest...)
	for _, f := range rd.Files {
		cand.Files = append(cand.Files, f.Path)
	}

	now := metav1.Now()
	from := &v1alpha1.RecoveredFrom{Path: cand.Path, By: by, At: now}
	if origin != nil {
		from.CommitSHA = origin.CommitSHA
		cand.CommitSHA = origin.CommitSHA
		if pr := origin.PullRequest; pr != nil {
			from.PullRequest = &v1alpha1.PullRequest{Number: origin.PRNumber, URL: pr.URL, State: "merged"}
			cand.PullRequest = pr.URL
		}
	}
	o := &v1alpha1.Order{
		ObjectMeta: metav1.ObjectMeta{
			Name: cand.OrderID, Namespace: namespace,
			Labels: map[string]string{v1alpha1.OrderIDKey: cand.OrderID},
		},
		Spec: v1alpha1.OrderSpec{
			Template:    cand.Template,
			RequestedBy: cand.RequestedBy,
			Reason:      "recovered from " + cand.Path,
			Placement:   v1alpha1.Placement{ManagementCluster: mc.Name},
			Delivery:    v1alpha1.DeliveryPullRequest,
			Rendered:    rd,
			Recovered:   from,
		},
	}
	if cand.RequestedBy != "" {
		o.Annotations = map[string]string{v1alpha1.RequestedByKey: cand.RequestedBy}
	}
	cand.order = o
	return nil
}

// carries reports whether a document of text is annotated with order-id id.
func carries(text, id string) bool {
	docs, err := layout.SplitDocuments(text)
	if err != nil {
		return false
	}
	for _, d := range docs {
		if d.Annotations[v1alpha1.OrderIDKey] == id {
			return true
		}
	}
	return false
}
