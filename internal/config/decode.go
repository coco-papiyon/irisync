package config

import (
	"fmt"
	"math"
	"net"
	"path"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type decoder func(*yaml.Node, string) error
type fields map[string]decoder

func invalid(field, reason string) error {
	return fmt.Errorf("invalid configuration: %s: %s", field, reason)
}

func dereference(node *yaml.Node) *yaml.Node {
	if node.Kind == yaml.AliasNode {
		return node.Alias
	}
	return node
}

func requireType(node *yaml.Node, kind yaml.Kind, tag, field, reason string) error {
	if node.Kind != kind || node.ShortTag() != tag {
		return invalid(field, reason)
	}
	return nil
}

func decodeMapping(node *yaml.Node, field string, ignoreUnknown bool, known fields) error {
	name := field
	if name == "" {
		name = "root"
	}
	if err := requireType(node, yaml.MappingNode, "!!map", name, "expected a mapping"); err != nil {
		return err
	}
	for i := 0; i < len(node.Content); i += 2 {
		key := dereference(node.Content[i])
		decode, found := known[key.Value]
		if !found {
			if key.Kind == yaml.ScalarNode && key.ShortTag() == "!!merge" {
				return fmt.Errorf("invalid configuration: %s: YAML merge key at line %d is unsupported", name, node.Content[i].Line)
			}
			if ignoreUnknown {
				continue
			}
			return fmt.Errorf("invalid configuration: %s: unknown key at line %d", name, node.Content[i].Line)
		}
		child := key.Value
		if field != "" {
			child = field + "." + child
		}
		if err := requireType(key, yaml.ScalarNode, "!!str", child, "expected a string key"); err != nil {
			return err
		}
		if err := decode(dereference(node.Content[i+1]), child); err != nil {
			return err
		}
	}
	return nil
}

func section(known fields) decoder {
	return func(node *yaml.Node, field string) error {
		return decodeMapping(node, field, false, known)
	}
}

func read[T any](out *T, parse func(*yaml.Node, string) (T, error)) decoder {
	return func(node *yaml.Node, field string) error {
		value, err := parse(node, field)
		if err == nil {
			*out = value
		}
		return err
	}
}

func stringValue(node *yaml.Node, field string) (string, error) {
	if err := requireType(node, yaml.ScalarNode, "!!str", field, "expected a string"); err != nil {
		return "", err
	}
	if strings.TrimSpace(node.Value) == "" || strings.ContainsRune(node.Value, 0) {
		return "", invalid(field, "expected a non-empty string without NUL")
	}
	return node.Value, nil
}

func boolValue(node *yaml.Node, field string) (bool, error) {
	if err := requireType(node, yaml.ScalarNode, "!!bool", field, "expected a boolean"); err != nil {
		return false, err
	}
	switch strings.ToLower(node.Value) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, invalid(field, "expected true or false")
	}
}

func httpListenValue(node *yaml.Node, field string) (string, error) {
	value, err := stringValue(node, field)
	if err != nil {
		return "", err
	}
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return "", invalid(field, "expected a host and port")
	}
	if host != "" && !validNetworkHost(host) {
		return "", invalid(field, "expected a valid host name or IP address")
	}
	number, err := strconv.ParseUint(port, 10, 16)
	if err != nil || number == 0 {
		return "", invalid(field, "expected a port in 1..65535")
	}
	return value, nil
}

func intValue(node *yaml.Node, field string) (int, error) {
	if err := requireType(node, yaml.ScalarNode, "!!int", field, "expected an integer"); err != nil {
		return 0, err
	}
	value, err := strconv.ParseUint(node.Value, 10, strconv.IntSize-1)
	if err != nil || value == 0 {
		return 0, invalid(field, "expected a positive decimal integer")
	}
	return int(value), nil
}

func durationValue(node *yaml.Node, field string) (time.Duration, error) {
	value, err := stringValue(node, field)
	if err != nil {
		return 0, err
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, invalid(field, "expected a positive duration with a unit")
	}
	return duration, nil
}

func sizeValue(node *yaml.Node, field string) (int64, error) {
	if node.Kind == yaml.ScalarNode {
		suffix := strings.TrimLeft(node.Value, "0123456789")
		tag := node.ShortTag()
		if tag == "!!str" || tag == "!!int" && suffix == "" {
			for i, unit := range []string{"B", "KiB", "MiB", "GiB", "TiB"} {
				if suffix == unit || suffix == "" && i == 0 {
					if size, ok := decimalSize(strings.TrimSuffix(node.Value, suffix), i); ok {
						return size, nil
					}
					break
				}
			}
		}
	}
	return 0, invalid(field, "expected non-negative bytes or an integer with B, KiB, MiB, GiB or TiB")
}

func decimalSize(number string, unit int) (int64, bool) {
	value, err := strconv.ParseUint(number, 10, 63)
	shift := unit * 10
	if err != nil || value > math.MaxInt64>>shift {
		return 0, false
	}
	return int64(value) << shift, true
}

func startupValue(node *yaml.Node, field string) (Startup, error) {
	value, err := stringValue(node, field)
	if err != nil {
		return "", err
	}
	switch startup := Startup(value); startup {
	case Start, Stop, Restore:
		return startup, nil
	default:
		return "", invalid(field, "expected start, stop or restore")
	}
}

func conflictPriorityValue(node *yaml.Node, field string) (ConflictPriority, error) {
	value, err := stringValue(node, field)
	if err != nil {
		return "", err
	}
	switch priority := ConflictPriority(value); priority {
	case PreferLocal, PreferRemote:
		return priority, nil
	default:
		return "", invalid(field, "expected local or remote")
	}
}

func excludeValue(node *yaml.Node, field string) ([]string, error) {
	if err := requireType(node, yaml.SequenceNode, "!!seq", field, "expected a sequence"); err != nil {
		return nil, err
	}
	patterns := make([]string, 0, len(node.Content))
	for i, item := range node.Content {
		element := fmt.Sprintf("%s[%d]", field, i)
		pattern, err := stringValue(dereference(item), element)
		if err != nil {
			return nil, err
		}
		for _, component := range strings.Split(pattern, "/") {
			switch component {
			case "", ".", "..":
				return nil, invalid(element, "expected a relative glob with non-empty path components")
			case "**":
				continue
			}
			if _, err := path.Match(component, ""); err != nil {
				return nil, invalid(element, "invalid glob syntax")
			}
		}
		patterns = append(patterns, pattern)
	}
	return patterns, nil
}
