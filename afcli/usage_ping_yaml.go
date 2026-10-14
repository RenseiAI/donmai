package afcli

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// usage_ping_yaml.go — raw-node edits for the `usagePing` daemon.yaml
// block. The project-tree YAML model (afclient.DaemonYAML) does not carry
// this block, so `host set usagePing.*` edits the file as a yaml.Node
// tree: only the named field inside `usagePing` changes and every other
// key — machine, orchestrator, autoUpdate, and anything the model does
// not know — is preserved verbatim.

// mintUsagePingID generates a cryptographically random installation id
// (16 random bytes, lowercase hex). It is created only on opt-in and is
// not derived from any machine, host, or organization identifier.
func mintUsagePingID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate installation id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// validUsagePingID reports whether id looks like a minted id.
func validUsagePingID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// readUsagePingField returns the scalar value of one field inside the
// top-level `usagePing` mapping, or "" when the block or field is
// absent.
func readUsagePingField(path, field string) (string, error) {
	doc, err := loadUsagePingDoc(path)
	if err != nil {
		return "", err
	}
	if doc == nil {
		return "", nil
	}
	block := findMappingValue(doc, "usagePing")
	if block == nil || block.Kind != yaml.MappingNode {
		return "", nil
	}
	value := findMappingValue(block, field)
	if value == nil || value.Kind != yaml.ScalarNode {
		return "", nil
	}
	return value.Value, nil
}

// writeUsagePingField sets one scalar field inside the top-level
// `usagePing` mapping, creating the block when absent. An empty value
// removes the field (but keeps the block); use deleteUsagePingBlock to
// drop the block entirely.
func writeUsagePingField(path, field, value string) error {
	doc, isNew, err := loadUsagePingDocForWrite(path)
	if err != nil {
		return err
	}
	block := findMappingValue(doc, "usagePing")
	if block == nil {
		block = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		doc.Content = append(doc.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "usagePing"},
			block,
		)
	}
	if block.Kind != yaml.MappingNode {
		return fmt.Errorf("daemon config: usagePing block is not a mapping")
	}
	if value == "" {
		deleteUsagePingKey(block, field)
	} else {
		tag := "!!str"
		if field == "enabled" {
			tag = "!!bool"
		}
		upsertUsagePingKey(block, field, &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value})
	}
	return storeUsagePingDoc(path, doc, isNew)
}

// deleteUsagePingBlock removes the top-level `usagePing` mapping
// entirely. Disabling the count leaves no identifier and no
// destination behind.
func deleteUsagePingBlock(path string) error {
	doc, isNew, err := loadUsagePingDocForWrite(path)
	if err != nil {
		return err
	}
	if isNew {
		return nil
	}
	deleteUsagePingKey(doc, "usagePing")
	return storeUsagePingDoc(path, doc, false)
}

// loadUsagePingDoc reads the daemon.yaml mapping for inspection. It
// returns (nil, nil) when the file does not exist.
func loadUsagePingDoc(path string) (*yaml.Node, error) {
	data, err := os.ReadFile(path) //nolint:gosec // operator-supplied path is intentional
	if err != nil {
		return nil, fmt.Errorf("read daemon config %q: %w", path, err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse daemon config %q: %w", path, err)
	}
	doc := &root
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		doc = doc.Content[0]
	}
	if doc.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("daemon config %q is not a mapping", path)
	}
	return doc, nil
}

// loadUsagePingDocForWrite reads the mapping for editing, returning a
// fresh empty mapping with isNew=true when the file does not exist.
func loadUsagePingDocForWrite(path string) (doc *yaml.Node, isNew bool, err error) {
	data, err := os.ReadFile(path) //nolint:gosec // operator-supplied path is intentional
	if err != nil {
		return nil, false, fmt.Errorf("read daemon config %q: %w", path, err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, false, fmt.Errorf("parse daemon config %q: %w", path, err)
	}
	doc = &root
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		doc = doc.Content[0]
	}
	if doc.Kind != yaml.MappingNode {
		return nil, false, fmt.Errorf("daemon config %q is not a mapping", path)
	}
	return doc, false, nil
}

// storeUsagePingDoc marshals the edited mapping back atomically
// (tmp file + rename), creating the parent directory as needed.
func storeUsagePingDoc(path string, doc *yaml.Node, _ bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal daemon config: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename temp config: %w", err)
	}
	return nil
}

// upsertUsagePingKey replaces (or appends) a key in a mapping node.
func upsertUsagePingKey(mapping *yaml.Node, key string, value *yaml.Node) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1] = value
			return
		}
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		value,
	)
}

// deleteUsagePingKey removes a key (and its value) from a mapping node.
func deleteUsagePingKey(mapping *yaml.Node, key string) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
			return
		}
	}
}

// findMappingValue returns the value node for key in a mapping node,
// following aliases. It returns nil when the key is absent.
func findMappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		k := mapping.Content[i]
		if k.Kind == yaml.ScalarNode && k.Value == key {
			v := mapping.Content[i+1]
			if v.Kind == yaml.AliasNode && v.Alias != nil {
				return v.Alias
			}
			return v
		}
	}
	return nil
}
