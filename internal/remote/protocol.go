// Package remote exchanges scan requests and results over an OpenSSH session.
package remote

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/coco-papiyon/irisync/internal/scan"
)

const version = 1
const scanFeature = "scan-sha256"
const maxFrame = 64 << 10
const maxScanBytes = 256 << 20

var (
	ErrConnection   = errors.New("SSH connection or agent startup failed")
	ErrIncompatible = errors.New("incompatible agent version or scan feature")
	ErrProtocol     = errors.New("invalid agent protocol message")
	ErrDisconnected = errors.New("SSH session disconnected before scan completion")
	ErrScan         = errors.New("remote scan failed")
	ErrScanTooLarge = errors.New("remote scan exceeded the response size limit")
)

type message struct {
	Type     string     `json:"type"`
	Version  int        `json:"version,omitempty"`
	Features []string   `json:"features,omitempty"`
	Root     string     `json:"root,omitempty"`
	File     *scanFile  `json:"file,omitempty"`
	Issue    *scanIssue `json:"issue,omitempty"`
	Code     string     `json:"code,omitempty"`
}

type scanFile struct {
	Path    string     `json:"path"`
	Size    *int64     `json:"size"`
	ModTime *time.Time `json:"mtime"`
	SHA256  []byte     `json:"sha256"`
}

type scanIssue struct {
	// Linux can contain filenames that are not UTF-8. Byte encoding keeps
	// those paths distinct in the stream when reporting an invalid name.
	Path []byte         `json:"path"`
	Kind scan.IssueKind `json:"kind"`
}

func hello() message {
	return message{Type: "hello", Version: version, Features: []string{scanFeature}}
}

func compatible(m message) bool {
	return m.Version == version && slices.Contains(m.Features, scanFeature)
}

func readMessage(in io.Reader) (message, int, error) {
	var header [4]byte
	var result message
	if _, err := io.ReadFull(in, header[:]); err != nil {
		return result, 0, err
	}
	size := int(binary.BigEndian.Uint32(header[:]))
	if size <= 0 || size > maxFrame {
		return result, 0, ErrProtocol
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(in, payload); err != nil {
		return result, 0, err
	}
	if !utf8.Valid(payload) || json.Unmarshal(payload, &result) != nil || result.Type == "" {
		return message{}, 0, ErrProtocol
	}
	return result, size + len(header), nil
}

func writeMessage(out io.Writer, msg message) error {
	payload, err := json.Marshal(msg)
	if err != nil || len(payload) > maxFrame {
		return ErrProtocol
	}
	frame := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(payload)))
	copy(frame[4:], payload)
	n, err := out.Write(frame)
	if err == nil && n != len(frame) {
		return io.ErrShortWrite
	}
	return err
}
