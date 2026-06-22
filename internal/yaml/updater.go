package yaml

import (
	"bytes"
	"errors"
	"io"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type PathPart struct {
	Key   string
	Index int // -1 if it's not a slice index
}

// ProcessYAMLUpdates processes multiple updates on a multi-document YAML content
func ProcessYAMLUpdates(yamlData []byte, updates map[string]string) ([]byte, error) {
	dec := yaml.NewDecoder(bytes.NewReader(yamlData))
	var documents []*yaml.Node
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		documents = append(documents, &doc)
	}

	for pathStr, newValue := range updates {
		parts := ParsePath(pathStr)
		updated := false
		for _, doc := range documents {
			if UpdateNode(doc, parts, newValue) {
				updated = true
			}
		}
		if !updated {
			return nil, errors.New("key path not found: " + pathStr)
		}
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	for _, doc := range documents {
		if err := enc.Encode(doc); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// ParsePath parses a dot-notation key path like "spec.template.spec.containers[0].image"
func ParsePath(pathStr string) []PathPart {
	parts := strings.Split(pathStr, ".")
	var result []PathPart
	for _, p := range parts {
		if idx := strings.Index(p, "["); idx != -1 && strings.HasSuffix(p, "]") {
			key := p[:idx]
			indexStr := p[idx+1 : len(p)-1]
			index, err := strconv.Atoi(indexStr)
			if err == nil {
				// Add the parent mapping key first
				if key != "" {
					result = append(result, PathPart{Key: key, Index: -1})
				}
				// Add the sequence index part
				result = append(result, PathPart{Key: "", Index: index})
				continue
			}
		}
		result = append(result, PathPart{Key: p, Index: -1})
	}
	return result
}

// UpdateNode recursively traverses yaml.Node to find the target path and updates its value
func UpdateNode(node *yaml.Node, parts []PathPart, newValue string) bool {
	if len(parts) == 0 {
		if node.Kind == yaml.ScalarNode {
			node.Value = newValue
			return true
		}
		return false
	}

	part := parts[0]
	switch node.Kind {
	case yaml.DocumentNode:
		for _, child := range node.Content {
			if UpdateNode(child, parts, newValue) {
				return true
			}
		}
	case yaml.MappingNode:
		for i := 0; i < len(node.Content); i += 2 {
			keyNode := node.Content[i]
			valueNode := node.Content[i+1]
			if keyNode.Value == part.Key {
				return UpdateNode(valueNode, parts[1:], newValue)
			}
		}
	case yaml.SequenceNode:
		if part.Index >= 0 && part.Index < len(node.Content) {
			return UpdateNode(node.Content[part.Index], parts[1:], newValue)
		}
	}
	return false
}
