package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"

	"gopkg.in/yaml.v3"
)

// Load reads a single YAML mapping and returns its root node.
func Load(path string) (*yaml.Node, error) {
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, errors.New("configuration file not found")
	case err != nil:
		return nil, errors.New("could not read configuration file")
	}

	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	switch err := decoder.Decode(&document); err {
	case io.EOF:
		return nil, errors.New("invalid YAML configuration: expected one mapping")
	case nil:
	default:
		// Parser errors can include input values.
		return nil, errors.New("invalid YAML configuration: syntax error")
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("invalid YAML configuration: expected one mapping")
	}
	var extra yaml.Node
	switch err := decoder.Decode(&extra); err {
	case nil:
		return nil, errors.New("invalid YAML configuration: multiple documents")
	case io.EOF:
	default:
		return nil, errors.New("invalid YAML configuration: syntax error")
	}
	root := document.Content[0]
	if err := checkKeys(root, make(map[*yaml.Node]*keyFingerprint)); err != nil {
		return nil, err
	}
	return root, nil
}

func checkKeys(node *yaml.Node, fingerprints map[*yaml.Node]*keyFingerprint) error {
	if node.Kind == yaml.MappingNode {
		if err := checkMappingKeys(node, fingerprints); err != nil {
			return err
		}
	}
	for _, child := range node.Content {
		if err := checkKeys(child, fingerprints); err != nil {
			return err
		}
	}
	return nil
}

func checkMappingKeys(mapping *yaml.Node, fingerprints map[*yaml.Node]*keyFingerprint) error {
	resolved := make(map[*yaml.Node]struct{}, len(mapping.Content)/2)
	for index := 0; index < len(mapping.Content); index += 2 {
		key := mapping.Content[index]
		target := key
		if key.Kind == yaml.AliasNode {
			target = key.Alias
		}
		if _, exists := resolved[target]; exists {
			return duplicateKey(key)
		}
		resolved[target] = struct{}{}
	}

	// Nodes that reach a cycle have no structural fingerprint. The identity
	// pass above also checks aliases that refer to those nodes.
	keys := make(map[keyFingerprint][]*yaml.Node, len(resolved))
	for index := 0; index < len(mapping.Content); index += 2 {
		key := mapping.Content[index]
		fingerprint := fingerprintKey(key, fingerprints)
		if fingerprint == nil {
			continue
		}
		for _, previous := range keys[*fingerprint] {
			comparison := keyComparator{fingerprints: fingerprints, results: make(map[keyComparison]bool)}
			if comparison.same(key, previous) {
				return duplicateKey(key)
			}
		}
		keys[*fingerprint] = append(keys[*fingerprint], key)
	}
	return nil
}

func duplicateKey(node *yaml.Node) error {
	return fmt.Errorf("invalid YAML configuration: duplicate key at line %d", node.Line)
}

type keyFingerprint [sha256.Size]byte

// A fixed-width pair keeps each mapping key bound to its value when sorted.
type entryFingerprint [2 * sha256.Size]byte

func fingerprintEntry(key, value *yaml.Node, fingerprints map[*yaml.Node]*keyFingerprint) (entryFingerprint, bool) {
	var entry entryFingerprint
	keyHash := fingerprintKey(key, fingerprints)
	valueHash := fingerprintKey(value, fingerprints)
	if keyHash == nil || valueHash == nil {
		return entry, false
	}
	copy(entry[:sha256.Size], keyHash[:])
	copy(entry[sha256.Size:], valueHash[:])
	return entry, true
}

func fingerprintKey(node *yaml.Node, fingerprints map[*yaml.Node]*keyFingerprint) *keyFingerprint {
	if node == nil {
		return nil
	}
	if fingerprint, found := fingerprints[node]; found {
		return fingerprint
	}
	// A revisit while the node is active returns nil. Keep nil cached for
	// any node that reaches a cycle.
	fingerprints[node] = nil
	if node.Kind == yaml.AliasNode {
		fingerprint := fingerprintKey(node.Alias, fingerprints)
		fingerprints[node] = fingerprint
		return fingerprint
	}

	digest := sha256.New()
	var size [8]byte
	writeUint := func(value uint64) {
		binary.BigEndian.PutUint64(size[:], value)
		digest.Write(size[:])
	}
	writeUint(uint64(node.Kind))
	writeUint(uint64(len(node.Tag)))
	io.WriteString(digest, node.Tag)
	writeUint(uint64(len(node.Value)))
	io.WriteString(digest, node.Value)
	writeUint(uint64(len(node.Content)))
	if node.Kind == yaml.MappingNode {
		entries := make([]entryFingerprint, 0, len(node.Content)/2)
		for i := 0; i < len(node.Content); i += 2 {
			entry, ok := fingerprintEntry(node.Content[i], node.Content[i+1], fingerprints)
			if !ok {
				return nil
			}
			entries = append(entries, entry)
		}
		slices.SortFunc(entries, func(a, b entryFingerprint) int { return bytes.Compare(a[:], b[:]) })
		for _, entry := range entries {
			digest.Write(entry[:])
		}
	} else {
		for _, child := range node.Content {
			fingerprint := fingerprintKey(child, fingerprints)
			if fingerprint == nil {
				return nil
			}
			digest.Write(fingerprint[:])
		}
	}
	fingerprint := new(keyFingerprint)
	digest.Sum(fingerprint[:0])
	fingerprints[node] = fingerprint
	return fingerprint
}

type keyComparison struct {
	left, right *yaml.Node
}

type keyComparator struct {
	fingerprints map[*yaml.Node]*keyFingerprint
	results      map[keyComparison]bool
}

func (c keyComparator) same(left, right *yaml.Node) (equal bool) {
	comparison := keyComparison{left, right}
	if equal, found := c.results[comparison]; found {
		return equal
	}
	c.results[comparison] = false
	defer func() { c.results[comparison] = equal }()

	if left == nil || right == nil {
		return false
	}
	if left.Kind == yaml.AliasNode {
		return c.same(left.Alias, right)
	}
	if right.Kind == yaml.AliasNode {
		return c.same(left, right.Alias)
	}
	if left.Kind != right.Kind || left.Tag != right.Tag || left.Value != right.Value ||
		len(left.Content) != len(right.Content) {
		return false
	}
	if left.Kind == yaml.MappingNode {
		return c.sameMapping(left.Content, right.Content)
	}
	for index, child := range left.Content {
		if !c.same(child, right.Content[index]) {
			return false
		}
	}
	return true
}

func (c keyComparator) sameMapping(left, right []*yaml.Node) bool {
	candidates := make(map[entryFingerprint][]int, len(right)/2)
	for i := 0; i < len(right); i += 2 {
		entry, ok := fingerprintEntry(right[i], right[i+1], c.fingerprints)
		if !ok {
			return false
		}
		candidates[entry] = append(candidates[entry], i)
	}
	for i := 0; i < len(left); i += 2 {
		entry, ok := fingerprintEntry(left[i], left[i+1], c.fingerprints)
		if !ok {
			return false
		}
		matches := candidates[entry]
		match := slices.IndexFunc(matches, func(index int) bool {
			return c.same(left[i], right[index]) && c.same(left[i+1], right[index+1])
		})
		if match < 0 {
			return false
		}
		// Each entry can match once, including when digests collide.
		last := len(matches) - 1
		matches[match] = matches[last]
		candidates[entry] = matches[:last]
	}
	return true
}
