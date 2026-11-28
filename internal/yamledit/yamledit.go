// Package yamledit adds a label or an annotation to one YAML document as a
// one-line text change: the rest of the document stays byte for byte as it
// was, so a pull request that adds an order-id (adopt, #4823) or the
// template annotation shows exactly that line and nothing else.
package yamledit

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// SetMetadata adds key: value under metadata.<field> (labels or
// annotations) of doc. changed is false when the key is there with that
// value already; a different value is an error, as are a flow-style map and
// a document without metadata.
func SetMetadata(doc, field, key, value string) (out string, changed bool, err error) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(doc), &root); err != nil {
		return "", false, fmt.Errorf("parsing the document: %w", err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return "", false, fmt.Errorf("the document is not a mapping")
	}
	metaKey, meta := lookup(root.Content[0], "metadata")
	if meta == nil || meta.Kind != yaml.MappingNode || meta.Style&yaml.FlowStyle != 0 || len(meta.Content) == 0 {
		return "", false, fmt.Errorf("the document has no block metadata")
	}
	scalar, err := yaml.Marshal(value)
	if err != nil {
		return "", false, err
	}
	val := strings.TrimSpace(string(scalar))

	lines := strings.Split(doc, "\n")
	insert := func(after int, add ...string) string {
		// after is a 1-based line number: insert behind that line.
		out := append([]string{}, lines[:after]...)
		out = append(out, add...)
		return strings.Join(append(out, lines[after:]...), "\n")
	}

	fieldKey, m := lookup(meta, field)
	if fieldKey == nil {
		ind := strings.Repeat(" ", meta.Content[0].Column-1)
		return insert(metaKey.Line, ind+field+":", ind+"  "+key+": "+val), true, nil
	}
	if m.Kind != yaml.MappingNode || m.Style&yaml.FlowStyle != 0 || len(m.Content) == 0 {
		return "", false, fmt.Errorf("metadata.%s is not a block map", field)
	}
	if _, v := lookup(m, key); v != nil {
		if v.Value == value {
			return doc, false, nil
		}
		return "", false, fmt.Errorf("metadata.%s.%s is %q already, not %q", field, key, v.Value, value)
	}
	ind := strings.Repeat(" ", m.Content[0].Column-1)
	return insert(fieldKey.Line, ind+key+": "+val), true, nil
}

func lookup(m *yaml.Node, key string) (*yaml.Node, *yaml.Node) {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil, nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i], m.Content[i+1]
		}
	}
	return nil, nil
}
