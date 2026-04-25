package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseSSHTarget_Happy(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want SSHTarget
	}{
		{"user@host", "user@host", SSHTarget{User: "user", Host: "host"}},
		{"host only", "host", SSHTarget{Host: "host"}},
		{"user@host:port", "user@host:2222", SSHTarget{User: "user", Host: "host", Port: 2222}},
		{"host:port", "host:2222", SSHTarget{Host: "host", Port: 2222}},
		{"deploy@10.0.0.1", "deploy@10.0.0.1", SSHTarget{User: "deploy", Host: "10.0.0.1"}},
		{"trim whitespace around", "  user@host  ", SSHTarget{User: "user", Host: "host"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSSHTarget(tt.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseSSHTarget(%q) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseSSHTarget_Errors(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		wantSubst string
	}{
		{"empty", "", "ssh target is empty"},
		{"only whitespace", "   ", "ssh target is empty"},
		{"contains whitespace", "a b", "must not contain whitespace"},
		{"empty user", "@host", "user is empty"},
		{"empty host after user", "user@", "host is empty"},
		{"port not a number", "user@host:abc", `port "abc" is not a number`},
		{"port zero", "user@host:0", "out of range"},
		{"port too big", "user@host:99999", "out of range"},
		{"ipv6 rejected (bracket)", "[::1]:22", "IPv6 not supported"},
		{"ipv6 bare loopback", "::1", "too many ':' separators"},
		{"ipv6 bare link-local", "fe80::1", "too many ':' separators"},
		{"too many colons typo", "host:22:30", "too many ':' separators"},
		{"trailing colon empty port", "host:", "port is empty"},
		{"user trailing colon empty port", "user@host:", "port is empty"},
		{"double @", "user@@host", "at most one '@'"},
		{"two @ separators", "user@host@host", "at most one '@'"},
		{"colon in user", "user:pass@host", "user must not contain ':'"},
		{"host starts with dash", "-F/tmp/cfg", "host must not start with '-'"},
		{"host starts with dash option", "-oProxyCommand=evil", "host must not start with '-'"},
		{"host starts with dash jump", "-Jjump", "host must not start with '-'"},
		{"user starts with dash", "-oFoo=bar@host", "user must not start with '-'"},
		{"user dash with port", "-x@host:22", "user must not start with '-'"},
		{"host dash with port", "-host:22", "host must not start with '-'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseSSHTarget(tt.in)
			if err == nil {
				t.Fatalf("expected error for %q, got nil", tt.in)
			}
			if !strings.Contains(err.Error(), tt.wantSubst) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantSubst)
			}
		})
	}
}

func TestSSHTarget_SSHHost(t *testing.T) {
	tests := []struct {
		name string
		in   SSHTarget
		want string
	}{
		{"with user", SSHTarget{User: "u", Host: "h"}, "u@h"},
		{"without user", SSHTarget{Host: "h"}, "h"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.SSHHost(); got != tt.want {
				t.Errorf("SSHHost() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSSHTarget_PortArgs(t *testing.T) {
	tests := []struct {
		name string
		in   SSHTarget
		want []string
	}{
		{"no port", SSHTarget{Port: 0}, nil},
		{"with port", SSHTarget{Port: 2222}, []string{"-p", "2222"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.in.PortArgs()
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("PortArgs() = %v, want %v", got, tt.want)
			}
		})
	}
}
