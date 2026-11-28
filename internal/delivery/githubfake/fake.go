// Package githubfake is an in-memory GitHub for tests: one repository with
// commits, refs, contents, pull requests and compare, enough for
// internal/delivery. Merge simulates the merge button.
package githubfake

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type commit struct {
	SHA     string
	Tree    string
	Parents []string
	Message string
	Seq     int
}

// PR is a pull request of the fake.
type PR struct {
	Number   int
	Title    string
	Body     string
	Head     string
	Base     string
	HeadSHA  string
	State    string // open, closed
	Merged   bool
	MergeSHA string
	Labels   []string
}

// Server is the fake. Owner/Repo is the only repository it serves.
type Server struct {
	*httptest.Server
	Owner, Repo string

	mu           sync.Mutex
	trees        map[string]map[string]string // tree sha -> path -> content
	commits      map[string]*commit
	refs         map[string]string // heads/main -> sha
	pulls        []*PR
	n            int
	checks       map[string][]Check // sha -> check runs
	mergeMethods []string           // of every merge through the API
}

// Check is one check run of the fake.
type Check struct {
	Name       string
	Status     string // queued, in_progress, completed
	Conclusion string // success, failure, skipped, ...
}

// New starts a fake whose main branch holds files.
func New(owner, repo string, files map[string]string) *Server {
	s := &Server{Owner: owner, Repo: repo, trees: map[string]map[string]string{}, commits: map[string]*commit{}, refs: map[string]string{}, checks: map[string][]Check{}}
	tree := s.putTree(files)
	c := s.putCommit(tree, nil, "initial")
	s.refs["heads/main"] = c
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

func hash(parts ...string) string {
	h := sha1.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (s *Server) putTree(files map[string]string) string {
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := []string{"tree"}
	for _, k := range keys {
		parts = append(parts, k, files[k])
	}
	sha := hash(parts...)
	cp := make(map[string]string, len(files))
	for k, v := range files {
		cp[k] = v
	}
	s.trees[sha] = cp
	return sha
}

func (s *Server) putCommit(tree string, parents []string, msg string) string {
	s.n++
	sha := hash("commit", tree, strings.Join(parents, ","), msg, strconv.Itoa(s.n))
	s.commits[sha] = &commit{SHA: sha, Tree: tree, Parents: parents, Message: msg, Seq: s.n}
	return sha
}

// File returns a file on a branch.
func (s *Server) File(branch, path string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.commits[s.refs["heads/"+branch]]
	if !ok {
		return "", false
	}
	v, ok := s.trees[c.Tree][path]
	return v, ok
}

// Head returns the SHA a branch points to.
func (s *Server) Head(branch string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refs["heads/"+branch]
}

// Message returns a commit's message.
func (s *Server) Message(sha string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.commits[sha]; ok {
		return c.Message
	}
	return ""
}

// Pulls returns a copy of the pull requests.
func (s *Server) Pulls() []PR {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PR, 0, len(s.pulls))
	for _, p := range s.pulls {
		out = append(out, *p)
	}
	return out
}

// MergeMethods returns the methods of the merges done through the API.
func (s *Server) MergeMethods() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.mergeMethods...)
}

// SetChecks sets the check runs of a commit (replacing earlier ones).
func (s *Server) SetChecks(sha string, checks ...Check) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checks[sha] = checks
}

// Merge merges pull request n into its base (a merge commit) and returns the
// merge SHA.
func (s *Server) Merge(n int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.merge(n)
}

func (s *Server) merge(n int) string {
	pr := s.pull(n)
	base := s.refs["heads/"+pr.Base]
	head := s.commits[pr.HeadSHA]
	files := map[string]string{}
	for k, v := range s.trees[s.commits[base].Tree] {
		files[k] = v
	}
	for k, v := range s.trees[head.Tree] {
		files[k] = v
	}
	// What the head removed against its parent goes from the base too.
	if len(head.Parents) > 0 {
		for k := range s.trees[s.commits[head.Parents[0]].Tree] {
			if _, ok := s.trees[head.Tree][k]; !ok {
				delete(files, k)
			}
		}
	}
	sha := s.putCommit(s.putTree(files), []string{base, pr.HeadSHA}, "Merge pull request #"+strconv.Itoa(n))
	s.refs["heads/"+pr.Base] = sha
	pr.State, pr.Merged, pr.MergeSHA = "closed", true, sha
	return sha
}

// Advance adds an unrelated commit on main and returns its SHA.
func (s *Server) Advance() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	base := s.refs["heads/main"]
	sha := s.putCommit(s.commits[base].Tree, []string{base}, "unrelated")
	s.refs["heads/main"] = sha
	return sha
}

func (s *Server) pull(n int) *PR {
	for _, p := range s.pulls {
		if p.Number == n {
			return p
		}
	}
	return nil
}

func (s *Server) prJSON(p *PR) map[string]any {
	var mergedAt any
	if p.Merged {
		mergedAt = "2026-11-22T10:00:00Z"
	}
	labels := []map[string]any{}
	for _, l := range p.Labels {
		labels = append(labels, map[string]any{"name": l})
	}
	return map[string]any{
		"number": p.Number, "title": p.Title, "body": p.Body, "state": p.State,
		"merged": p.Merged, "merge_commit_sha": p.MergeSHA, "merged_at": mergedAt, "labels": labels,
		"html_url": fmt.Sprintf("https://github.com/%s/%s/pull/%d", s.Owner, s.Repo, p.Number),
		"head":     map[string]any{"ref": p.Head, "sha": p.HeadSHA},
		"base":     map[string]any{"ref": p.Base},
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func notFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
}

// ancestor reports whether a is b or an ancestor of b.
func (s *Server) ancestor(a, b string) bool {
	seen := map[string]bool{}
	stack := []string{b}
	for len(stack) > 0 {
		c := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if c == a {
			return true
		}
		if seen[c] {
			continue
		}
		seen[c] = true
		if cm, ok := s.commits[c]; ok {
			stack = append(stack, cm.Parents...)
		}
	}
	return false
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := "/repos/" + s.Owner + "/" + s.Repo + "/"
	p := strings.TrimPrefix(r.URL.Path, "/api/v3")
	if !strings.HasPrefix(p, prefix) {
		notFound(w)
		return
	}
	rest := strings.TrimPrefix(p, prefix)

	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(rest, "git/ref/"):
		ref := strings.TrimPrefix(rest, "git/ref/")
		sha, ok := s.refs[ref]
		if !ok {
			notFound(w)
			return
		}
		writeJSON(w, 200, map[string]any{"ref": "refs/" + ref, "object": map[string]any{"sha": sha, "type": "commit"}})

	case r.Method == http.MethodPost && rest == "git/refs":
		var in struct{ Ref, SHA string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		ref := strings.TrimPrefix(in.Ref, "refs/")
		if _, ok := s.refs[ref]; ok {
			writeJSON(w, 422, map[string]any{"message": "Reference already exists"})
			return
		}
		if _, ok := s.commits[in.SHA]; !ok {
			writeJSON(w, 422, map[string]any{"message": "Object does not exist"})
			return
		}
		s.refs[ref] = in.SHA
		writeJSON(w, 201, map[string]any{"ref": in.Ref, "object": map[string]any{"sha": in.SHA}})

	case r.Method == http.MethodDelete && strings.HasPrefix(rest, "git/refs/"):
		ref := strings.TrimPrefix(rest, "git/refs/")
		if _, ok := s.refs[ref]; !ok {
			writeJSON(w, 422, map[string]any{"message": "Reference does not exist"})
			return
		}
		delete(s.refs, ref)
		w.WriteHeader(204)

	case r.Method == http.MethodGet && strings.HasPrefix(rest, "git/commits/"):
		c, ok := s.commits[strings.TrimPrefix(rest, "git/commits/")]
		if !ok {
			notFound(w)
			return
		}
		parents := []map[string]any{}
		for _, p := range c.Parents {
			parents = append(parents, map[string]any{"sha": p})
		}
		writeJSON(w, 200, map[string]any{"sha": c.SHA, "message": c.Message, "tree": map[string]any{"sha": c.Tree}, "parents": parents})

	case r.Method == http.MethodPost && rest == "git/trees":
		var in struct {
			BaseTree string `json:"base_tree"`
			Tree     []struct {
				Path    string  `json:"path"`
				Content *string `json:"content"`
				SHA     *string `json:"sha"`
			} `json:"tree"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		files := map[string]string{}
		for k, v := range s.trees[in.BaseTree] {
			files[k] = v
		}
		for _, e := range in.Tree {
			if e.Content == nil && e.SHA == nil {
				delete(files, e.Path)
				continue
			}
			if e.Content != nil {
				files[e.Path] = *e.Content
			}
		}
		writeJSON(w, 201, map[string]any{"sha": s.putTree(files)})

	case r.Method == http.MethodPost && rest == "git/commits":
		var in struct {
			Message string   `json:"message"`
			Tree    string   `json:"tree"`
			Parents []string `json:"parents"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if _, ok := s.trees[in.Tree]; !ok {
			writeJSON(w, 422, map[string]any{"message": "tree does not exist"})
			return
		}
		sha := s.putCommit(in.Tree, in.Parents, in.Message)
		writeJSON(w, 201, map[string]any{"sha": sha, "message": in.Message})

	case r.Method == http.MethodGet && strings.HasPrefix(rest, "contents/"):
		path := strings.TrimPrefix(rest, "contents/")
		ref := r.URL.Query().Get("ref")
		if ref == "" {
			ref = s.refs["heads/main"]
		} else if sha, ok := s.refs["heads/"+ref]; ok {
			ref = sha
		}
		c, ok := s.commits[ref]
		if !ok {
			notFound(w)
			return
		}
		v, ok := s.trees[c.Tree][path]
		if !ok {
			if entries := dirEntries(s.trees[c.Tree], path); len(entries) > 0 {
				writeJSON(w, 200, entries)
				return
			}
			notFound(w)
			return
		}
		writeJSON(w, 200, map[string]any{"type": "file", "path": path, "encoding": "base64",
			"content": base64.StdEncoding.EncodeToString([]byte(v))})

	case r.Method == http.MethodGet && rest == "pulls":
		head := strings.TrimPrefix(r.URL.Query().Get("head"), s.Owner+":")
		state := r.URL.Query().Get("state")
		out := []map[string]any{}
		for _, p := range s.pulls {
			if head != "" && p.Head != head {
				continue
			}
			if state != "all" && p.State != "open" {
				continue
			}
			out = append(out, s.prJSON(p))
		}
		writeJSON(w, 200, out)

	case r.Method == http.MethodPost && rest == "pulls":
		var in struct{ Title, Head, Base, Body string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		sha, ok := s.refs["heads/"+in.Head]
		if !ok {
			writeJSON(w, 422, map[string]any{"message": "head does not exist"})
			return
		}
		for _, p := range s.pulls {
			if p.Head == in.Head && p.State == "open" {
				writeJSON(w, 422, map[string]any{"message": "A pull request already exists"})
				return
			}
		}
		pr := &PR{Number: len(s.pulls) + 1, Title: in.Title, Body: in.Body, Head: in.Head, Base: in.Base, HeadSHA: sha, State: "open"}
		s.pulls = append(s.pulls, pr)
		writeJSON(w, 201, s.prJSON(pr))

	case r.Method == http.MethodGet && strings.HasPrefix(rest, "commits/") && strings.HasSuffix(rest, "/check-runs"):
		sha := strings.TrimSuffix(strings.TrimPrefix(rest, "commits/"), "/check-runs")
		runs := []map[string]any{}
		for i, c := range s.checks[sha] {
			runs = append(runs, map[string]any{"id": i + 1, "name": c.Name, "status": c.Status, "conclusion": c.Conclusion})
		}
		writeJSON(w, 200, map[string]any{"total_count": len(runs), "check_runs": runs})

	case r.Method == http.MethodGet && strings.HasPrefix(rest, "commits/") && strings.HasSuffix(rest, "/status"):
		writeJSON(w, 200, map[string]any{"state": "pending", "statuses": []any{}})

	case r.Method == http.MethodGet && strings.HasPrefix(rest, "commits/") && strings.HasSuffix(rest, "/pulls"):
		sha := strings.TrimSuffix(strings.TrimPrefix(rest, "commits/"), "/pulls")
		out := []map[string]any{}
		for _, p := range s.pulls {
			if p.HeadSHA == sha || (p.Merged && p.MergeSHA == sha) {
				out = append(out, s.prJSON(p))
			}
		}
		writeJSON(w, 200, out)

	case r.Method == http.MethodGet && strings.HasPrefix(rest, "commits/"):
		c, ok := s.commits[strings.TrimPrefix(rest, "commits/")]
		if !ok {
			notFound(w)
			return
		}
		files := []map[string]any{}
		for _, f := range s.changed(c) {
			files = append(files, map[string]any{"filename": f.path, "status": f.status})
		}
		writeJSON(w, 200, map[string]any{"sha": c.SHA, "commit": map[string]any{"message": c.Message}, "files": files})

	case r.Method == http.MethodGet && rest == "commits":
		ref := r.URL.Query().Get("sha")
		if sha, ok := s.refs["heads/"+ref]; ok {
			ref = sha
		}
		if _, ok := s.commits[ref]; !ok {
			notFound(w)
			return
		}
		path := r.URL.Query().Get("path")
		var hist []*commit
		for sha := range s.commits {
			c := s.commits[sha]
			if !s.ancestor(sha, ref) {
				continue
			}
			for _, f := range s.changed(c) {
				if path == "" || f.path == path {
					hist = append(hist, c)
					break
				}
			}
		}
		sort.Slice(hist, func(i, j int) bool { return hist[i].Seq > hist[j].Seq })
		out := []map[string]any{}
		for _, c := range hist {
			out = append(out, map[string]any{"sha": c.SHA, "commit": map[string]any{"message": c.Message}})
		}
		writeJSON(w, 200, out)

	case r.Method == http.MethodPut && strings.HasPrefix(rest, "pulls/") && strings.HasSuffix(rest, "/merge"):
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rest, "pulls/"), "/merge"))
		pr := s.pull(n)
		if err != nil || pr == nil {
			notFound(w)
			return
		}
		var in struct {
			SHA         string `json:"sha"`
			MergeMethod string `json:"merge_method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		switch {
		case pr.State != "open":
			writeJSON(w, 405, map[string]any{"message": "Pull Request is not mergeable"})
		case in.SHA != "" && in.SHA != pr.HeadSHA:
			writeJSON(w, 409, map[string]any{"message": "Head branch was modified"})
		default:
			s.mergeMethods = append(s.mergeMethods, in.MergeMethod)
			sha := s.merge(n)
			writeJSON(w, 200, map[string]any{"sha": sha, "merged": true})
		}

	case r.Method == http.MethodPost && strings.HasPrefix(rest, "issues/") && strings.HasSuffix(rest, "/labels"):
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rest, "issues/"), "/labels"))
		pr := s.pull(n)
		if err != nil || pr == nil {
			notFound(w)
			return
		}
		// go-github sends a bare array; the API also takes {"labels": [...]}.
		var raw json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&raw)
		var labels []string
		if json.Unmarshal(raw, &labels) != nil {
			var in struct{ Labels []string }
			_ = json.Unmarshal(raw, &in)
			labels = in.Labels
		}
		for _, l := range labels {
			if !slices.Contains(pr.Labels, l) {
				pr.Labels = append(pr.Labels, l)
			}
		}
		writeJSON(w, 200, []any{})

	case strings.HasPrefix(rest, "pulls/"):
		n, err := strconv.Atoi(strings.TrimPrefix(rest, "pulls/"))
		pr := s.pull(n)
		if err != nil || pr == nil {
			notFound(w)
			return
		}
		if r.Method == http.MethodPatch {
			var in struct{ State string }
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in.State != "" && !pr.Merged {
				pr.State = in.State
			}
		}
		writeJSON(w, 200, s.prJSON(pr))

	case r.Method == http.MethodGet && strings.HasPrefix(rest, "compare/"):
		base, head, ok := strings.Cut(strings.TrimPrefix(rest, "compare/"), "...")
		if _, ok1 := s.commits[base]; !ok || !ok1 {
			notFound(w)
			return
		}
		if _, ok := s.commits[head]; !ok {
			notFound(w)
			return
		}
		status := "diverged"
		switch {
		case base == head:
			status = "identical"
		case s.ancestor(base, head):
			status = "ahead"
		case s.ancestor(head, base):
			status = "behind"
		}
		writeJSON(w, 200, map[string]any{"status": status})

	default:
		notFound(w)
	}
}

type change struct{ path, status string }

// changed lists what c changed against its first parent.
func (s *Server) changed(c *commit) []change {
	before := map[string]string{}
	if len(c.Parents) > 0 {
		before = s.trees[s.commits[c.Parents[0]].Tree]
	}
	after := s.trees[c.Tree]
	var out []change
	for k, v := range after {
		old, ok := before[k]
		switch {
		case !ok:
			out = append(out, change{k, "added"})
		case old != v:
			out = append(out, change{k, "modified"})
		}
	}
	for k := range before {
		if _, ok := after[k]; !ok {
			out = append(out, change{k, "removed"})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

// dirEntries lists the files and directories directly in dir.
func dirEntries(tree map[string]string, dir string) []map[string]any {
	prefix := strings.TrimSuffix(dir, "/") + "/"
	seen := map[string]bool{}
	var out []map[string]any
	keys := make([]string, 0, len(tree))
	for k := range tree {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		rest, ok := strings.CutPrefix(k, prefix)
		if !ok {
			continue
		}
		name, _, sub := strings.Cut(rest, "/")
		if seen[name] {
			continue
		}
		seen[name] = true
		typ := "file"
		if sub {
			typ = "dir"
		}
		out = append(out, map[string]any{"type": typ, "name": name, "path": prefix + name})
	}
	return out
}

// Branches returns the branch names.
func (s *Server) Branches() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for ref := range s.refs {
		if b, ok := strings.CutPrefix(ref, "heads/"); ok {
			out = append(out, b)
		}
	}
	sort.Strings(out)
	return out
}

// Commit adds a commit with files changed (nil content deletes) on top of
// branch, creating the branch from main when it does not exist, and returns
// its SHA.
func (s *Server) Commit(branch, message string, files map[string]*string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	parent, ok := s.refs["heads/"+branch]
	if !ok {
		parent = s.refs["heads/main"]
	}
	tree := map[string]string{}
	for k, v := range s.trees[s.commits[parent].Tree] {
		tree[k] = v
	}
	for k, v := range files {
		if v == nil {
			delete(tree, k)
		} else {
			tree[k] = *v
		}
	}
	sha := s.putCommit(s.putTree(tree), []string{parent}, message)
	s.refs["heads/"+branch] = sha
	return sha
}

// ClosePR closes pull request n without merging, as a reviewer would.
func (s *Server) ClosePR(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pr := s.pull(n); pr != nil && !pr.Merged {
		pr.State = "closed"
	}
}
