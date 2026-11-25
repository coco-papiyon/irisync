package config

import (
	"fmt"
	"os"
	"runtime"
	"time"

	"gopkg.in/yaml.v3"
)

type Startup string

const (
	Start   Startup = "start"
	Stop    Startup = "stop"
	Restore Startup = "restore"
)

type ConflictPriority string

const (
	PreferLocal  ConflictPriority = "local"
	PreferRemote ConflictPriority = "remote"
)

type Config struct {
	HTTPListen  string
	Sync        Sync
	Performance Performance
	Pairs       []Pair
}

type Sync struct {
	MinDelay         time.Duration
	IdleDelay        time.Duration
	MaxDelay         time.Duration
	MaxFileSize      int64 // Bytes. Zero means unlimited.
	FullScanInterval time.Duration
	RetryInterval    time.Duration
}

type Performance struct {
	MaxConcurrentPairs  int
	MaxTransfersPerPair int
}

type Pair struct {
	ID               string
	Startup          Startup
	LocalPath        string
	RemotePath       string
	SSH              SSH
	Exclude          []string
	MaxFileSize      int64 // Effective limit in bytes. Zero means unlimited.
	ConflictPriority ConflictPriority
}

type SSH struct {
	Host         string
	UseSSHConfig bool
	User         string
	Port         int // Zero leaves port selection to OpenSSH.
	IdentityFile string
}

// Load returns the effective configuration only after all settings and pairs
// have passed validation.
func Load(path string) (*Config, error) {
	root, err := readMapping(path)
	if err != nil {
		return nil, err
	}
	return decodeConfig(root, os.LookupEnv)
}

func decodeConfig(root *yaml.Node, lookupEnv func(string) (string, bool)) (*Config, error) {
	cfg := Config{
		HTTPListen: "127.0.0.1:8080",
		Sync: Sync{
			MinDelay:         60 * time.Second,
			IdleDelay:        5 * time.Second,
			MaxDelay:         120 * time.Second,
			MaxFileSize:      1 << 30,
			FullScanInterval: 30 * time.Minute,
			RetryInterval:    5 * time.Second,
		},
		Performance: Performance{MaxConcurrentPairs: 5, MaxTransfersPerPair: 2},
	}

	var pairs *yaml.Node
	err := decodeMapping(root, "", true, fields{
		"server": section(fields{
			"http": section(fields{"listen": read(&cfg.HTTPListen, httpListenValue)}),
		}),
		"sync": section(fields{
			"min_delay":          read(&cfg.Sync.MinDelay, durationValue),
			"idle_delay":         read(&cfg.Sync.IdleDelay, durationValue),
			"max_delay":          read(&cfg.Sync.MaxDelay, durationValue),
			"max_file_size":      read(&cfg.Sync.MaxFileSize, sizeValue),
			"full_scan_interval": read(&cfg.Sync.FullScanInterval, durationValue),
			"retry_interval":     read(&cfg.Sync.RetryInterval, durationValue),
		}),
		"performance": section(fields{
			"max_concurrent_pairs":   read(&cfg.Performance.MaxConcurrentPairs, intValue),
			"max_transfers_per_pair": read(&cfg.Performance.MaxTransfersPerPair, intValue),
		}),
		"pairs": func(node *yaml.Node, field string) error {
			if err := requireType(node, yaml.SequenceNode, "!!seq", field, "expected a sequence"); err != nil {
				return err
			}
			pairs = node
			return nil
		},
	})
	if err != nil {
		return nil, err
	}
	if err := cfg.applyEnv(lookupEnv); err != nil {
		return nil, err
	}
	if cfg.Sync.MinDelay > cfg.Sync.MaxDelay {
		return nil, invalid("sync.min_delay", "must not exceed sync.max_delay")
	}
	if pairs != nil {
		cfg.Pairs, err = decodePairs(pairs, cfg.Sync.MaxFileSize)
		if err != nil {
			return nil, err
		}
	}
	return &cfg, nil
}

func (cfg *Config) applyEnv(lookup func(string) (string, bool)) error {
	for _, override := range []struct {
		name  string
		tag   string
		apply decoder
	}{
		{"IRISYNC_HTTP_LISTEN", "!!str", read(&cfg.HTTPListen, httpListenValue)},
		{"IRISYNC_MAX_FILE_SIZE", "!!str", read(&cfg.Sync.MaxFileSize, sizeValue)},
		{"IRISYNC_FULL_SCAN_INTERVAL", "!!str", read(&cfg.Sync.FullScanInterval, durationValue)},
		{"IRISYNC_MAX_CONCURRENT_PAIRS", "!!int", read(&cfg.Performance.MaxConcurrentPairs, intValue)},
	} {
		if value, ok := lookup(override.name); ok {
			node := &yaml.Node{Kind: yaml.ScalarNode, Tag: override.tag, Value: value}
			if err := override.apply(node, override.name); err != nil {
				return err
			}
		}
	}
	return nil
}

type pairRoots struct {
	local       rootPath
	remote      []rootPath
	destination sshDestination
}

func decodePairs(sequence *yaml.Node, maxFileSize int64) ([]Pair, error) {
	pairs := make([]Pair, 0, len(sequence.Content))
	roots := make([]pairRoots, 0, len(sequence.Content))
	ids := make(map[string]int, len(sequence.Content))
	for i, node := range sequence.Content {
		field := fmt.Sprintf("pairs[%d]", i)
		pair, err := decodePair(dereference(node), field, maxFileSize)
		if err != nil {
			return nil, err
		}
		if previous, found := ids[pair.ID]; found {
			return nil, fmt.Errorf("invalid configuration: %s.id duplicates pairs[%d].id", field, previous)
		}

		local, ok := parseRoot(pair.LocalPath, runtime.GOOS == "windows")
		if !ok {
			return nil, invalid(field+".local.path", "expected an absolute path for the controller OS")
		}
		remote := parseRemoteRoots(pair.RemotePath)
		if len(remote) == 0 {
			return nil, invalid(field+".remote.path", "expected an absolute POSIX or Windows path")
		}
		destination := pair.SSH.destination()
		for j, previous := range roots {
			if local.overlaps(previous.local) {
				return nil, fmt.Errorf("invalid configuration: %s.local.path overlaps pairs[%d].local.path", field, j)
			}
			if destination == previous.destination && remoteRootsOverlap(remote, previous.remote) {
				return nil, fmt.Errorf("invalid configuration: %s.remote.path overlaps pairs[%d].remote.path", field, j)
			}
		}

		ids[pair.ID] = i
		roots = append(roots, pairRoots{local: local, remote: remote, destination: destination})
		pairs = append(pairs, pair)
	}
	return pairs, nil
}

func decodePair(node *yaml.Node, field string, maxFileSize int64) (Pair, error) {
	pair := Pair{Startup: Restore, MaxFileSize: maxFileSize, ConflictPriority: PreferLocal}
	err := decodeMapping(node, field, false, fields{
		"id":      read(&pair.ID, stringValue),
		"startup": read(&pair.Startup, startupValue),
		"local": section(fields{
			"path": read(&pair.LocalPath, stringValue),
		}),
		"remote": section(fields{
			"path": read(&pair.RemotePath, stringValue),
			"ssh":  read(&pair.SSH, sshValue),
		}),
		"exclude": read(&pair.Exclude, excludeValue),
		"sync": section(fields{
			"max_file_size": read(&pair.MaxFileSize, sizeValue),
		}),
		"conflict": section(fields{
			"priority": read(&pair.ConflictPriority, conflictPriorityValue),
		}),
	})
	if err != nil {
		return Pair{}, err
	}
	for _, required := range []struct{ name, value string }{
		{"id", pair.ID},
		{"local.path", pair.LocalPath},
		{"remote.path", pair.RemotePath},
		{"remote.ssh", pair.SSH.Host},
	} {
		if required.value == "" {
			return Pair{}, invalid(field+"."+required.name, "required")
		}
	}
	return pair, nil
}
