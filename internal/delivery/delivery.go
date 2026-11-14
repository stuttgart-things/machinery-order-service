// Package delivery writes an order's files to the GitOps repository through
// the GitHub API and follows its pull request. It never clones: the commit is
// built from the base branch with the git data API (tree + commit + ref), so
// the service needs no disk and no git binary.
//
// Every commit carries an "Order-Id: <id>" trailer. The trailer is how the
// service recognises its own branch and pull request on a retry, and how an
// Order can be found again from git.
package delivery

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/google/go-github/v84/github"

	"github.com/stuttgart-things/machinery-order-service/internal/layout"
)

// TrailerKey is the commit trailer naming the order.
const TrailerKey = "Order-Id"

// File is one file of an order.
type File struct {
	Path    string
	Content string
}

// Request is one order to deliver as a pull request.
type Request struct {
	// Repository is owner/name.
	Repository string
	BaseBranch string
	Branch     string
	Title      string
	Body       string
	OrderID    string
	Files      []File
}

// Result is what Open did.
type Result struct {
	CommitSHA      string
	PRNumber       int
	PRURL          string
	Kustomizations []string
}

// PRState is a pull request as GitHub reports it.
type PRState struct {
	// State is open, closed or merged.
	State    string
	URL      string
	HeadSHA  string
	MergeSHA string
}

// Deliverer is what the controller needs from git. *GitHub implements it.
type Deliverer interface {
	Open(ctx context.Context, r Request) (*Result, error)
	PullRequest(ctx context.Context, repository string, number int) (*PRState, error)
	Close(ctx context.Context, repository string, number int, branch string) error
	// Contains reports whether commit sha is head or one of its ancestors.
	Contains(ctx context.Context, repository, sha, head string) (bool, error)
}

// ConflictError means the order cannot be delivered as it is: a file or the
// branch is taken by something else. Retrying does not help.
type ConflictError struct{ Msg string }

func (e *ConflictError) Error() string { return e.Msg }

// GitHub delivers through the GitHub API.
type GitHub struct {
	Client *github.Client
}

// New returns a GitHub deliverer for the API at apiURL ("" = api.github.com)
// using httpClient, whose transport adds the token.
func New(apiURL string, httpClient *http.Client) (*GitHub, error) {
	c := github.NewClient(httpClient)
	if apiURL != "" && strings.TrimRight(apiURL, "/") != "https://api.github.com" {
		u, err := url.Parse(strings.TrimRight(apiURL, "/") + "/")
		if err != nil {
			return nil, fmt.Errorf("GitHub API URL: %w", err)
		}
		c.BaseURL = u
	}
	return &GitHub{Client: c}, nil
}

func split(repository string) (string, string, error) {
	owner, name, ok := strings.Cut(repository, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("repository must be owner/name, got %q", repository)
	}
	return owner, name, nil
}

func notFound(resp *github.Response, err error) bool {
	if err == nil {
		return false
	}
	var er *github.ErrorResponse
	if errors.As(err, &er) && er.Response != nil && er.Response.StatusCode == http.StatusNotFound {
		return true
	}
	return resp != nil && resp.StatusCode == http.StatusNotFound
}

// HasTrailer reports whether a commit message carries "Order-Id: id".
func HasTrailer(message, id string) bool {
	for _, line := range strings.Split(message, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(k), TrailerKey) && strings.TrimSpace(v) == id {
			return true
		}
	}
	return false
}

// CommitMessage is the title plus the Order-Id trailer.
func CommitMessage(title, id string) string {
	return title + "\n\n" + TrailerKey + ": " + id + "\n"
}

func (g *GitHub) commitIsOurs(ctx context.Context, owner, repo, sha, id string) (bool, error) {
	c, _, err := g.Client.Git.GetCommit(ctx, owner, repo, sha)
	if err != nil {
		return false, fmt.Errorf("reading commit %s: %w", sha, err)
	}
	return HasTrailer(c.GetMessage(), id), nil
}

// Open delivers r as a pull request. It is idempotent: a branch or pull
// request that is already the order's own (Order-Id trailer) is reused, so
// the controller may call it again after a partial failure.
func (g *GitHub) Open(ctx context.Context, r Request) (*Result, error) {
	owner, repo, err := split(r.Repository)
	if err != nil {
		return nil, err
	}
	if r.Branch == "" || r.BaseBranch == "" || r.OrderID == "" || len(r.Files) == 0 {
		return nil, errors.New("delivery: branch, base branch, order ID and files are required")
	}

	// 1. A pull request of this order exists already.
	prs, _, err := g.Client.PullRequests.List(ctx, owner, repo, &github.PullRequestListOptions{
		State: "all", Head: owner + ":" + r.Branch, Base: r.BaseBranch,
	})
	if err != nil {
		return nil, fmt.Errorf("listing pull requests of %s: %w", r.Branch, err)
	}
	for _, pr := range prs {
		ours, err := g.commitIsOurs(ctx, owner, repo, pr.GetHead().GetSHA(), r.OrderID)
		if err != nil {
			return nil, err
		}
		if ours {
			return &Result{CommitSHA: pr.GetHead().GetSHA(), PRNumber: pr.GetNumber(), PRURL: pr.GetHTMLURL()}, nil
		}
		if pr.GetState() == "open" {
			return nil, &ConflictError{Msg: fmt.Sprintf("branch %s has an open pull request of another order: %s", r.Branch, pr.GetHTMLURL())}
		}
	}

	// 2. The branch exists: ours (a retry after the push) or taken.
	res := &Result{}
	ref, resp, err := g.Client.Git.GetRef(ctx, owner, repo, "heads/"+r.Branch)
	switch {
	case err == nil:
		sha := ref.GetObject().GetSHA()
		ours, err := g.commitIsOurs(ctx, owner, repo, sha, r.OrderID)
		if err != nil {
			return nil, err
		}
		if !ours {
			return nil, &ConflictError{Msg: fmt.Sprintf("branch %s exists already and is not this order's", r.Branch)}
		}
		res.CommitSHA = sha
	case notFound(resp, err):
		if err := g.commit(ctx, owner, repo, r, res); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("reading branch %s: %w", r.Branch, err)
	}

	// 3. The pull request.
	pr, _, err := g.Client.PullRequests.Create(ctx, owner, repo, &github.NewPullRequest{
		Title: github.Ptr(r.Title),
		Head:  github.Ptr(r.Branch),
		Base:  github.Ptr(r.BaseBranch),
		Body:  github.Ptr(r.Body),
	})
	if err != nil {
		return nil, fmt.Errorf("opening the pull request: %w", err)
	}
	res.PRNumber, res.PRURL = pr.GetNumber(), pr.GetHTMLURL()
	return res, nil
}

// commit builds the order's commit on top of the base branch and creates the
// branch.
func (g *GitHub) commit(ctx context.Context, owner, repo string, r Request, res *Result) error {
	baseRef, _, err := g.Client.Git.GetRef(ctx, owner, repo, "heads/"+r.BaseBranch)
	if err != nil {
		return fmt.Errorf("reading %s: %w", r.BaseBranch, err)
	}
	baseSHA := baseRef.GetObject().GetSHA()
	baseCommit, _, err := g.Client.Git.GetCommit(ctx, owner, repo, baseSHA)
	if err != nil {
		return fmt.Errorf("reading %s: %w", baseSHA, err)
	}

	var entries []*github.TreeEntry
	for _, f := range r.Files {
		if err := layout.ValidatePath(f.Path); err != nil {
			return &ConflictError{Msg: err.Error()}
		}
		exists, _, err := g.read(ctx, owner, repo, f.Path, baseSHA)
		if err != nil {
			return err
		}
		if exists {
			return &ConflictError{Msg: fmt.Sprintf("%s exists already on %s: the name is taken", f.Path, r.BaseBranch)}
		}
		entries = append(entries, blob(f.Path, f.Content))
	}

	// The kustomization next to each file gets its entry, as in create-vm.
	kustomizations := map[string]string{}
	var order []string
	for _, f := range r.Files {
		dir := path.Dir(f.Path)
		for _, n := range layout.KustomizationNames {
			kp := path.Join(dir, n)
			raw, seen := kustomizations[kp]
			if !seen {
				var exists bool
				exists, raw, err = g.read(ctx, owner, repo, kp, baseSHA)
				if err != nil {
					return err
				}
				if !exists {
					continue
				}
				order = append(order, kp)
			}
			after, _, err := layout.AppendEntry(kp, raw, path.Base(f.Path))
			if err != nil {
				return &ConflictError{Msg: err.Error()}
			}
			kustomizations[kp] = after
			break
		}
	}
	for _, kp := range order {
		entries = append(entries, blob(kp, kustomizations[kp]))
		res.Kustomizations = append(res.Kustomizations, kp)
	}

	tree, _, err := g.Client.Git.CreateTree(ctx, owner, repo, baseCommit.GetTree().GetSHA(), entries)
	if err != nil {
		return fmt.Errorf("creating the tree: %w", err)
	}
	commit, _, err := g.Client.Git.CreateCommit(ctx, owner, repo, github.Commit{
		Message: github.Ptr(CommitMessage(r.Title, r.OrderID)),
		Tree:    tree,
		Parents: []*github.Commit{{SHA: github.Ptr(baseSHA)}},
	}, nil)
	if err != nil {
		return fmt.Errorf("creating the commit: %w", err)
	}
	if _, _, err := g.Client.Git.CreateRef(ctx, owner, repo, github.CreateRef{Ref: "refs/heads/" + r.Branch, SHA: commit.GetSHA()}); err != nil {
		return fmt.Errorf("creating branch %s: %w", r.Branch, err)
	}
	res.CommitSHA = commit.GetSHA()
	return nil
}

func blob(p, content string) *github.TreeEntry {
	return &github.TreeEntry{Path: github.Ptr(p), Mode: github.Ptr("100644"), Type: github.Ptr("blob"), Content: github.Ptr(content)}
}

// read returns whether p exists at ref and its content.
func (g *GitHub) read(ctx context.Context, owner, repo, p, ref string) (bool, string, error) {
	fc, dir, resp, err := g.Client.Repositories.GetContents(ctx, owner, repo, p, &github.RepositoryContentGetOptions{Ref: ref})
	if notFound(resp, err) {
		return false, "", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("reading %s: %w", p, err)
	}
	if fc == nil {
		if dir != nil {
			return true, "", &ConflictError{Msg: fmt.Sprintf("%s is a directory", p)}
		}
		return false, "", nil
	}
	content, err := fc.GetContent()
	if err != nil {
		return false, "", fmt.Errorf("decoding %s: %w", p, err)
	}
	return true, content, nil
}

// PullRequest reads a pull request.
func (g *GitHub) PullRequest(ctx context.Context, repository string, number int) (*PRState, error) {
	owner, repo, err := split(repository)
	if err != nil {
		return nil, err
	}
	pr, _, err := g.Client.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("reading pull request #%d: %w", number, err)
	}
	st := &PRState{State: pr.GetState(), URL: pr.GetHTMLURL(), HeadSHA: pr.GetHead().GetSHA()}
	if pr.GetMerged() {
		st.State = "merged"
		st.MergeSHA = pr.GetMergeCommitSHA()
	}
	return st, nil
}

// Close closes the pull request and deletes its branch.
func (g *GitHub) Close(ctx context.Context, repository string, number int, branch string) error {
	owner, repo, err := split(repository)
	if err != nil {
		return err
	}
	if number > 0 {
		if _, _, err := g.Client.PullRequests.Edit(ctx, owner, repo, number, &github.PullRequest{State: github.Ptr("closed")}); err != nil {
			return fmt.Errorf("closing pull request #%d: %w", number, err)
		}
	}
	if branch != "" {
		resp, err := g.Client.Git.DeleteRef(ctx, owner, repo, "heads/"+branch)
		if err != nil && !notFound(resp, err) && (resp == nil || resp.StatusCode != http.StatusUnprocessableEntity) {
			return fmt.Errorf("deleting branch %s: %w", branch, err)
		}
	}
	return nil
}

// Contains reports whether sha is head or an ancestor of it.
func (g *GitHub) Contains(ctx context.Context, repository, sha, head string) (bool, error) {
	if sha == "" || head == "" {
		return false, nil
	}
	if sha == head {
		return true, nil
	}
	owner, repo, err := split(repository)
	if err != nil {
		return false, err
	}
	cmp, resp, err := g.Client.Repositories.CompareCommits(ctx, owner, repo, sha, head, nil)
	if notFound(resp, err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("comparing %s...%s: %w", sha, head, err)
	}
	s := cmp.GetStatus()
	return s == "identical" || s == "ahead", nil
}
