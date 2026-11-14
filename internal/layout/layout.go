// Package layout places the documents of a rendered order into the GitOps
// repository by the paths the order names itself. It follows the claims CLI's
// source-path layout (stuttgart-things/claims#92, internal/layout there) so a
// PR from the service matches one from `claims render --layout source-path`:
//
//   - a document annotated stuttgart-things.com/target-path goes to that file
//   - a document annotated stuttgart-things.com/source-path goes to that file
//   - a document with neither goes to the file of the document before it
//
// Paths are relative to the repository root. If a kustomization.yaml sits next
// to a written file, the file's entry is appended to its resources.
package layout

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// AnnotationSourcePath names the file a document lives in.
	AnnotationSourcePath = "stuttgart-things.com/source-path"
	// AnnotationTargetPath names the file a document goes to when it is not
	// the order's own file (e.g. a dev VM's ScrapeConfig).
	AnnotationTargetPath = "stuttgart-things.com/target-path"
	// AnnotationManagementCluster names the cluster that applies the order.
	AnnotationManagementCluster = "stuttgart-things.com/management-cluster"
)

// Document is one YAML document of a rendered order.
type Document struct {
	APIVersion  string
	Kind        string
	Name        string
	Namespace   string
	Labels      map[string]string
	Annotations map[string]string
	Content     string
}

// File is one file of an order: its repository-relative path and the
// documents it holds, in render order.
type File struct {
	Path      string
	Documents []Document
}

// Content is the file as written: every document starts with "---".
func (f File) Content() string {
	var sb strings.Builder
	for _, d := range f.Documents {
		sb.WriteString("---\n")
		sb.WriteString(strings.TrimSpace(d.Content))
		sb.WriteString("\n")
	}
	return sb.String()
}

var separator = regexp.MustCompile(`(?m)^---[ \t]*$`)

// SplitDocuments splits a YAML stream into its non-empty documents.
func SplitDocuments(content string) ([]Document, error) {
	var docs []Document
	for _, part := range separator.Split(content, -1) {
		var probe any
		if err := yaml.Unmarshal([]byte(part), &probe); err != nil {
			return nil, fmt.Errorf("parsing rendered document: %w", err)
		}
		if probe == nil {
			continue // empty or comments only
		}
		var meta struct {
			APIVersion string `yaml:"apiVersion"`
			Kind       string `yaml:"kind"`
			Metadata   struct {
				Name        string            `yaml:"name"`
				Namespace   string            `yaml:"namespace"`
				Labels      map[string]string `yaml:"labels"`
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(part), &meta); err != nil {
			return nil, fmt.Errorf("parsing rendered document: %w", err)
		}
		docs = append(docs, Document{
			APIVersion:  meta.APIVersion,
			Kind:        meta.Kind,
			Name:        meta.Metadata.Name,
			Namespace:   meta.Metadata.Namespace,
			Labels:      meta.Metadata.Labels,
			Annotations: meta.Metadata.Annotations,
			Content:     part,
		})
	}
	return docs, nil
}

// Plan maps the documents of a rendered order to files.
func Plan(content string) ([]File, error) {
	docs, err := SplitDocuments(content)
	if err != nil {
		return nil, err
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("the rendered order has no documents")
	}

	var files []File
	index := map[string]int{}
	current := ""
	for _, d := range docs {
		p := d.Annotations[AnnotationTargetPath]
		if p == "" {
			p = d.Annotations[AnnotationSourcePath]
		}
		if p == "" {
			p = current
		}
		if p == "" {
			return nil, fmt.Errorf("%s %q names no %s or %s: the order cannot be placed",
				d.Kind, d.Name, AnnotationSourcePath, AnnotationTargetPath)
		}
		if err := ValidatePath(p); err != nil {
			return nil, fmt.Errorf("%s %q: %w", d.Kind, d.Name, err)
		}
		if i, ok := index[p]; ok {
			files[i].Documents = append(files[i].Documents, d)
		} else {
			index[p] = len(files)
			files = append(files, File{Path: p, Documents: []Document{d}})
		}
		// A target-path document is placed elsewhere; the documents after it
		// still belong to the order's own file.
		if d.Annotations[AnnotationTargetPath] == "" {
			current = p
		}
	}
	return files, nil
}

// XR is the order's main object: the first document that names its own file
// (stuttgart-things.com/source-path).
func XR(files []File) (Document, bool) {
	for _, f := range files {
		for _, d := range f.Documents {
			if d.Annotations[AnnotationSourcePath] != "" {
				return d, true
			}
		}
	}
	return Document{}, false
}

// ValidatePath checks that p is a clean, relative .yaml path.
func ValidatePath(p string) error {
	if path.IsAbs(p) || strings.HasPrefix(p, "/") {
		return fmt.Errorf("path %q is absolute, want one relative to the repository root", p)
	}
	if clean := path.Clean(p); clean != p || clean == "." || strings.HasPrefix(clean, "../") || clean == ".." {
		return fmt.Errorf("path %q is not a clean path inside the repository", p)
	}
	if ext := path.Ext(p); ext != ".yaml" && ext != ".yml" {
		return fmt.Errorf("path %q is not a .yaml file", p)
	}
	return nil
}

// KustomizationNames are the file names kustomize reads, in its order.
var KustomizationNames = []string{"kustomization.yaml", "kustomization.yml", "Kustomization"}

// AppendEntry returns raw with "- entry" appended to its resources, the way
// the Backstage templates do it: one line at the end of the file, which needs
// resources: to be the last key. The second result is the appended line, or
// "" when the entry is there already.
func AppendEntry(kPath, raw, entry string) (string, string, error) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(raw), &root); err != nil {
		return "", "", fmt.Errorf("parsing %s: %w", kPath, err)
	}
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return "", "", fmt.Errorf("%s is not a YAML mapping", kPath)
	}
	m := root.Content[0]
	var resources *yaml.Node
	last := ""
	for i := 0; i+1 < len(m.Content); i += 2 {
		last = m.Content[i].Value
		if last == "resources" {
			resources = m.Content[i+1]
		}
	}
	if resources != nil && resources.Kind == yaml.SequenceNode {
		for _, r := range resources.Content {
			if r.Value == entry {
				return raw, "", nil
			}
		}
	}
	if resources == nil || last != "resources" || resources.Kind != yaml.SequenceNode ||
		resources.Style == yaml.FlowStyle || len(resources.Content) == 0 {
		return "", "", fmt.Errorf("%s: cannot append %q -- resources: must be the last key, a non-empty block list", kPath, entry)
	}

	indent := "  "
	for _, line := range strings.Split(raw, "\n") {
		if t := strings.TrimLeft(line, " "); strings.HasPrefix(t, "- ") {
			indent = line[:len(line)-len(t)]
		}
	}
	line := indent + "- " + entry
	after := raw
	if !strings.HasSuffix(after, "\n") {
		after += "\n"
	}
	return after + line + "\n", line, nil
}
