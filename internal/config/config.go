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
			if sameKey(key, previous, make(map[keyComparison]bool), make(map[keyComparison]bool)) {
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

func fingerprintKey(node *yaml.Node, fingerprints map[*yaml.Node]*keyFingerprint) *keyFingerprint {
	if node == nil {
		return nil
	}
	if fingerprint, found := fingerprints[node]; found {
		return fingerprint
	}
	// Mark the node active before following edges. A revisit returns nil because
	// recursive aliases cannot establish a finite match in sameKey. Leave nil
	// cached for any node that reaches a cycle.
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
	for _, child := range node.Content {
		fingerprint := fingerprintKey(child, fingerprints)
		if fingerprint == nil {
			return nil
		}
		digest.Write(fingerprint[:])
	}
	fingerprint := new(keyFingerprint)
	digest.Sum(fingerprint[:0])
	fingerprints[node] = fingerprint
	return fingerprint
}

type keyComparison struct {
	left, right *yaml.Node
}

func sameKey(left, right *yaml.Node, active, completed map[keyComparison]bool) (equal bool) {
	comparison := keyComparison{left, right}
	if equal, found := completed[comparison]; found {
		return equal
	}
	if active[comparison] {
		// A recursive alias cannot establish a finite structural match.
		return false
	}
	active[comparison] = true
	defer func() {
		delete(active, comparison)
		completed[comparison] = equal
	}()

	if left == nil || right == nil {
		return false
	}
	if left.Kind == yaml.AliasNode {
		return sameKey(left.Alias, right, active, completed)
	}
	if right.Kind == yaml.AliasNode {
		return sameKey(left, right.Alias, active, completed)
	}
	if left.Kind != right.Kind || left.Tag != right.Tag || left.Value != right.Value ||
		len(left.Content) != len(right.Content) {
		return false
	}
	for index, child := range left.Content {
		if !sameKey(child, right.Content[index], active, completed) {
			return false
		}
	}
	return true
}
