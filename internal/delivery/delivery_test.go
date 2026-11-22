package delivery_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stuttgart-things/machinery-order-service/internal/delivery"
	"github.com/stuttgart-things/machinery-order-service/internal/delivery/githubfake"
)

const kust = "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - other.yaml\n"

func setup(t *testing.T) (*githubfake.Server, *delivery.GitHub) {
	t.Helper()
	fake := githubfake.New("stuttgart-things", "stuttgart-things", map[string]string{
		"clusters/labda/vsphere/machinery-orders-xrs/kustomization.yaml": kust,
		"clusters/labda/vsphere/machinery-orders-xrs/other.yaml":         "kind: A\n",
		"clusters/labda/vsphere/machinery-orders-xrs/taken.yaml":         "kind: A\n",
	})
	t.Cleanup(fake.Close)
	g, err := delivery.New(fake.URL+"/", http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	return fake, g
}

func request(id string) delivery.Request {
	return delivery.Request{
		Repository: "stuttgart-things/stuttgart-things", BaseBranch: "main", Branch: "vm-demo",
		Title: "feat(machinery): VM demo (baseos, labda-vsphere) (MERGE = BAU)", Body: "body", OrderID: id,
		Files: []delivery.File{
			{Path: "clusters/labda/vsphere/machinery-orders-xrs/demo.yaml", Content: "---\nkind: NativeVsphereVM\n"},
			{Path: "clusters/labda/vsphere/sthings-infra/monitoring/host-demo.yaml", Content: "---\nkind: ScrapeConfig\n"},
		},
	}
}

func TestOpenMergeContains(t *testing.T) {
	fake, g := setup(t)
	ctx := context.Background()

	res, err := g.Open(ctx, request("ord-1"))
	if err != nil {
		t.Fatal(err)
	}
	if res.PRNumber != 1 || res.CommitSHA == "" || len(res.Kustomizations) != 1 {
		t.Fatalf("result: %+v", res)
	}
	if got, _ := fake.File("vm-demo", "clusters/labda/vsphere/machinery-orders-xrs/kustomization.yaml"); got != kust+"  - demo.yaml\n" {
		t.Errorf("kustomization: %q", got)
	}
	if _, ok := fake.File("vm-demo", "clusters/labda/vsphere/sthings-infra/monitoring/host-demo.yaml"); !ok {
		t.Error("extra file missing")
	}
	if !delivery.HasTrailer(fake.Message(res.CommitSHA), "ord-1") {
		t.Errorf("no trailer in %q", fake.Message(res.CommitSHA))
	}

	// A retry finds the same pull request.
	again, err := g.Open(ctx, request("ord-1"))
	if err != nil || again.PRNumber != 1 || again.CommitSHA != res.CommitSHA {
		t.Fatalf("retry: %+v %v", again, err)
	}
	if n := len(fake.Pulls()); n != 1 {
		t.Errorf("want 1 pull request, got %d", n)
	}

	// Another order on the same branch is refused.
	var ce *delivery.ConflictError
	if _, err := g.Open(ctx, request("ord-2")); !errors.As(err, &ce) {
		t.Errorf("want a conflict, got %v", err)
	}

	st, err := g.PullRequest(ctx, "stuttgart-things/stuttgart-things", 1)
	if err != nil || st.State != "open" {
		t.Fatalf("pr: %+v %v", st, err)
	}
	merge := fake.Merge(1)
	st, _ = g.PullRequest(ctx, "stuttgart-things/stuttgart-things", 1)
	if st.State != "merged" || st.MergeSHA != merge {
		t.Errorf("merged pr: %+v", st)
	}

	later := fake.Advance()
	for _, tc := range []struct {
		sha, head string
		want      bool
	}{{merge, merge, true}, {merge, later, true}, {later, merge, false}} {
		got, err := g.Contains(ctx, "stuttgart-things/stuttgart-things", tc.sha, tc.head)
		if err != nil || got != tc.want {
			t.Errorf("Contains(%s, %s) = %v %v", tc.sha[:7], tc.head[:7], got, err)
		}
	}
}

func TestOpenRefusesTakenFile(t *testing.T) {
	_, g := setup(t)
	r := request("ord-1")
	r.Files = []delivery.File{{Path: "clusters/labda/vsphere/machinery-orders-xrs/taken.yaml", Content: "x"}}
	var ce *delivery.ConflictError
	if _, err := g.Open(context.Background(), r); !errors.As(err, &ce) {
		t.Errorf("want a conflict, got %v", err)
	}
}

func TestClose(t *testing.T) {
	fake, g := setup(t)
	ctx := context.Background()
	res, err := g.Open(ctx, request("ord-1"))
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Close(ctx, "stuttgart-things/stuttgart-things", res.PRNumber, "vm-demo"); err != nil {
		t.Fatal(err)
	}
	if fake.Pulls()[0].State != "closed" || fake.Head("vm-demo") != "" {
		t.Errorf("not closed: %+v head %q", fake.Pulls()[0], fake.Head("vm-demo"))
	}
	// Closing twice is fine.
	if err := g.Close(ctx, "stuttgart-things/stuttgart-things", res.PRNumber, "vm-demo"); err != nil {
		t.Error(err)
	}
	// A new order may now reuse the branch name.
	if res2, err := g.Open(ctx, request("ord-2")); err != nil || res2.PRNumber != 2 {
		t.Errorf("reuse: %+v %v", res2, err)
	}
}

const repoName = "stuttgart-things/stuttgart-things"

// A merged order's branch left behind does not block the same name (#20).
func TestOpenHealsStaleBranch(t *testing.T) {
	fake, g := setup(t)
	ctx := context.Background()
	if _, err := g.Open(ctx, request("ord-1")); err != nil {
		t.Fatal(err)
	}
	fake.Merge(1)
	// The claim was deleted later; the branch vm-demo is still there.
	fake.Commit("main", "delete", map[string]*string{
		"clusters/labda/vsphere/machinery-orders-xrs/demo.yaml":          nil,
		"clusters/labda/vsphere/sthings-infra/monitoring/host-demo.yaml": nil,
	})
	res, err := g.Open(ctx, request("ord-2"))
	if err != nil {
		t.Fatalf("stale branch not healed: %v", err)
	}
	if res.PRNumber != 2 || !delivery.HasTrailer(fake.Message(fake.Head("vm-demo")), "ord-2") {
		t.Errorf("result %+v, head %q", res, fake.Message(fake.Head("vm-demo")))
	}
}

// A foreign branch with commits that are not on the base is a conflict.
func TestOpenForeignBranchWithCommits(t *testing.T) {
	fake, g := setup(t)
	x := "x"
	fake.Commit("vm-demo", "someone's work", map[string]*string{"notes.txt": &x})
	var ce *delivery.ConflictError
	if _, err := g.Open(context.Background(), request("ord-1")); !errors.As(err, &ce) {
		t.Fatalf("want a conflict, got %v", err)
	}
	if fake.Head("vm-demo") == "" {
		t.Error("deleted a branch with unmerged commits")
	}
}

func teardown(id string) delivery.Request {
	r := request(id)
	r.Branch, r.Title, r.Labels = "delete-claim/demo-vm", "Delete claim: NativeVsphereVM/demo-vm", []string{"delete-claim"}
	for i := range r.Files {
		r.Files[i].Delete, r.Files[i].Content = true, ""
	}
	return r
}

func TestOpenTeardown(t *testing.T) {
	fake, g := setup(t)
	ctx := context.Background()
	if _, err := g.Open(ctx, request("ord-1")); err != nil {
		t.Fatal(err)
	}
	fake.Merge(1)

	res, err := g.Open(ctx, teardown("ord-1"))
	if err != nil {
		t.Fatal(err)
	}
	pr := fake.Pulls()[1]
	if pr.Head != "delete-claim/demo-vm" || len(pr.Labels) != 1 || pr.Labels[0] != "delete-claim" {
		t.Errorf("pr %+v", pr)
	}
	if _, ok := fake.File("delete-claim/demo-vm", "clusters/labda/vsphere/machinery-orders-xrs/demo.yaml"); ok {
		t.Error("file still on the teardown branch")
	}
	if k, _ := fake.File("delete-claim/demo-vm", "clusters/labda/vsphere/machinery-orders-xrs/kustomization.yaml"); k != kust {
		t.Errorf("kustomization %q", k)
	}
	if len(res.Kustomizations) != 1 {
		t.Errorf("kustomizations %v", res.Kustomizations)
	}

	// Withdrawn (closed unmerged, branch gone): asking again opens a new PR.
	fake.ClosePR(res.PRNumber)
	if err := g.DeleteBranch(ctx, repoName, "delete-claim/demo-vm"); err != nil {
		t.Fatal(err)
	}
	again, err := g.Open(ctx, teardown("ord-1"))
	if err != nil || again.PRNumber != 3 {
		t.Fatalf("second attempt: %+v %v", again, err)
	}
	// A retry of the second attempt finds it.
	if retry, err := g.Open(ctx, teardown("ord-1")); err != nil || retry.PRNumber != 3 {
		t.Fatalf("retry: %+v %v", retry, err)
	}
	fake.Merge(3)
	if _, ok := fake.File("main", "clusters/labda/vsphere/machinery-orders-xrs/demo.yaml"); ok {
		t.Error("file still on main after the teardown merge")
	}

	// Nothing left to remove.
	if err := g.DeleteBranch(ctx, repoName, "delete-claim/demo-vm"); err != nil {
		t.Fatal(err)
	}
	r := teardown("ord-9")
	var ce *delivery.ConflictError
	if _, err := g.Open(ctx, r); !errors.As(err, &ce) {
		t.Errorf("want a conflict, got %v", err)
	}
}
