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
