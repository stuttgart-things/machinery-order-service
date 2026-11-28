package delivery

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/google/go-github/v84/github"
)

// RepoFile is one file as it is on a branch.
type RepoFile struct {
	Path    string
	Content string
}

// Dir reads the YAML files directly in dir at ref (no subdirectories). A
// directory that does not exist is empty.
func (g *GitHub) Dir(ctx context.Context, repository, ref, dir string) ([]RepoFile, error) {
	owner, repo, err := split(repository)
	if err != nil {
		return nil, err
	}
	_, entries, resp, err := g.Client.Repositories.GetContents(ctx, owner, repo, dir, &github.RepositoryContentGetOptions{Ref: ref})
	if notFound(resp, err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", dir, err)
	}
	var out []RepoFile
	for _, e := range entries {
		p := e.GetPath()
		if e.GetType() != "file" || (path.Ext(p) != ".yaml" && path.Ext(p) != ".yml") {
			continue
		}
		ok, content, err := g.read(ctx, owner, repo, p, ref)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, RepoFile{Path: p, Content: content})
		}
	}
	return out, nil
}

// File reads one file at ref; ok is false when it does not exist.
func (g *GitHub) File(ctx context.Context, repository, ref, p string) (string, bool, error) {
	owner, repo, err := split(repository)
	if err != nil {
		return "", false, err
	}
	ok, content, err := g.read(ctx, owner, repo, p, ref)
	return content, ok, err
}

// Origin is the commit that brought an order to the base branch.
type Origin struct {
	// CommitSHA is the commit with the order's Order-Id trailer.
	CommitSHA string
	// Files are the paths that commit added or changed.
	Files []string
	// PullRequest is its merged pull request, nil when none was found.
	PullRequest *PRState
	// PRNumber is the pull request's number (0 without one).
	PRNumber int
}

// originDepth bounds how far back Origin looks in a file's history.
const originDepth = 100

// Origin finds, in the history of p on ref, the commit that carries the
// trailer "Order-Id: id" (a squash merge, or the order's own commit behind a
// merge commit), the files it changed and its merged pull request. It is
// nil when no commit of the last 100 that touched p carries the trailer.
func (g *GitHub) Origin(ctx context.Context, repository, ref, p, id string) (*Origin, error) {
	owner, repo, err := split(repository)
	if err != nil {
		return nil, err
	}
	commits, _, err := g.Client.Repositories.ListCommits(ctx, owner, repo, &github.CommitsListOptions{
		SHA: ref, Path: p, ListOptions: github.ListOptions{PerPage: originDepth},
	})
	if err != nil {
		return nil, fmt.Errorf("history of %s: %w", p, err)
	}
	// newest first: the oldest commit with the trailer brought the order in
	sha := ""
	for _, c := range commits {
		if HasTrailer(c.GetCommit().GetMessage(), id) {
			sha = c.GetSHA()
		}
	}
	if sha == "" {
		return nil, nil
	}
	c, _, err := g.Client.Repositories.GetCommit(ctx, owner, repo, sha, nil)
	if err != nil {
		return nil, fmt.Errorf("reading commit %s: %w", sha, err)
	}
	o := &Origin{CommitSHA: sha}
	for _, f := range c.Files {
		if f.GetStatus() != "removed" {
			o.Files = append(o.Files, f.GetFilename())
		}
	}
	prs, _, err := g.Client.PullRequests.ListPullRequestsWithCommit(ctx, owner, repo, sha, nil)
	if err != nil {
		return nil, fmt.Errorf("pull requests of %s: %w", sha, err)
	}
	base := strings.TrimPrefix(ref, "refs/heads/")
	for _, pr := range prs {
		if pr.MergedAt == nil || pr.GetBase().GetRef() != base {
			continue
		}
		o.PRNumber = pr.GetNumber()
		o.PullRequest = &PRState{State: "merged", URL: pr.GetHTMLURL(), HeadSHA: pr.GetHead().GetSHA(), MergeSHA: pr.GetMergeCommitSHA()}
		break
	}
	return o, nil
}
