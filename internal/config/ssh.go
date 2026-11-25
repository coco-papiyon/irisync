package config

import (
	"net/netip"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

func sshValue(node *yaml.Node, field string) (SSH, error) {
	ssh := SSH{UseSSHConfig: true}
	err := decodeMapping(node, field, false, fields{
		"host":           read(&ssh.Host, stringValue),
		"use_ssh_config": read(&ssh.UseSSHConfig, boolValue),
		"user":           read(&ssh.User, stringValue),
		"port":           read(&ssh.Port, intValue),
		"identity_file":  read(&ssh.IdentityFile, stringValue),
	})
	if err != nil {
		return SSH{}, err
	}
	if !validSSHHost(ssh.Host, ssh.UseSSHConfig) {
		return SSH{}, invalid(field+".host", "expected an SSH host")
	}
	if ssh.UseSSHConfig {
		for _, setting := range []struct {
			name    string
			present bool
		}{
			{"user", ssh.User != ""},
			{"port", ssh.Port != 0},
			{"identity_file", ssh.IdentityFile != ""},
		} {
			if setting.present {
				return SSH{}, invalid(field+"."+setting.name, "requires use_ssh_config: false")
			}
		}
		return ssh, nil
	}
	if ssh.User == "" || strings.ContainsFunc(ssh.User, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) {
		return SSH{}, invalid(field+".user", "expected a non-empty user without whitespace or control characters")
	}
	if ssh.Port > 65535 {
		return SSH{}, invalid(field+".port", "expected a port in 1..65535")
	}
	if ssh.Port == 0 {
		ssh.Port = 22
	}
	return ssh, nil
}

func validSSHHost(host string, useSSHConfig bool) bool {
	if !useSSHConfig {
		if _, err := netip.ParseAddr(host); err == nil {
			// The parser also accepts IPv6 scope names. Check these like host names.
			_, zone, scoped := strings.Cut(host, "%")
			return !scoped || validHostName(zone)
		}
	}
	return validHostName(host)
}

func validHostName(host string) bool {
	if host == "" || strings.HasPrefix(host, "-") || strings.Trim(host, ".") == "" {
		return false
	}
	return !strings.ContainsFunc(host, func(r rune) bool {
		return !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' ||
			r == '.' || r == '-' || r == '_')
	})
}

type sshDestination struct {
	host         string
	port         int
	useSSHConfig bool
}

func (ssh SSH) destination() sshDestination {
	host := strings.ToLower(ssh.Host)
	if addr, err := netip.ParseAddr(ssh.Host); !ssh.UseSSHConfig && err == nil {
		host = addr.Unmap().String()
	}
	return sshDestination{host: host, port: ssh.Port, useSSHConfig: ssh.UseSSHConfig}
}
