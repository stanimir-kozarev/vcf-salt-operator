package salt_test

import (
	"testing"

	"github.com/stanimir-kozarev/vcf-salt-operator/pkg/salt"
)

func TestFindMinionID(t *testing.T) {
	tests := []struct {
		name        string
		vmName      string
		vmIP        string
		pendingKeys []string
		wantMinion  string
		wantMethod  salt.MatchMethod
	}{
		{
			name:        "exact name match",
			vmName:      "vm-web-01",
			vmIP:        "10.1.2.3",
			pendingKeys: []string{"vm-web-01", "vm-db-02"},
			wantMinion:  "vm-web-01",
			wantMethod:  salt.MatchByName,
		},
		{
			name:        "name match wins over IP match",
			vmName:      "vm-web-01",
			vmIP:        "10.1.2.3",
			pendingKeys: []string{"10.1.2.3", "vm-web-01"},
			wantMinion:  "vm-web-01",
			wantMethod:  salt.MatchByName,
		},
		{
			name:        "IP match when no name found",
			vmName:      "vm-web-01",
			vmIP:        "192.168.10.50",
			pendingKeys: []string{"10.0.0.1", "192.168.10.50"},
			wantMinion:  "192.168.10.50",
			wantMethod:  salt.MatchByIP,
		},
		{
			name:        "shifted IP match — first octet rotated",
			vmName:      "vm-unknown",
			vmIP:        "10.192.168.100",
			pendingKeys: []string{"192.168.100.10"},
			wantMinion:  "192.168.100.10",
			wantMethod:  salt.MatchByShiftedIP,
		},
		{
			name:        "shifted IP match with real-world pattern",
			vmName:      "some-vm",
			vmIP:        "10.0.5.200",
			pendingKeys: []string{"0.5.200.10"},
			wantMinion:  "0.5.200.10",
			wantMethod:  salt.MatchByShiftedIP,
		},
		{
			name:        "prefix match — FQDN minion ID",
			vmName:      "vm-db-01",
			vmIP:        "172.16.0.5",
			pendingKeys: []string{"vm-db-01.corp.local", "other-vm.corp.local"},
			wantMinion:  "vm-db-01.corp.local",
			wantMethod:  salt.MatchByPrefix,
		},
		{
			name:        "no match returns MatchNone",
			vmName:      "vm-missing",
			vmIP:        "1.2.3.4",
			pendingKeys: []string{"vm-other", "vm-another"},
			wantMinion:  "",
			wantMethod:  salt.MatchNone,
		},
		{
			name:        "empty pending keys list",
			vmName:      "vm-web-01",
			vmIP:        "10.0.0.1",
			pendingKeys: []string{},
			wantMinion:  "",
			wantMethod:  salt.MatchNone,
		},
		{
			name:        "nil pending keys list",
			vmName:      "vm-web-01",
			vmIP:        "10.0.0.1",
			pendingKeys: nil,
			wantMinion:  "",
			wantMethod:  salt.MatchNone,
		},
		{
			name:        "empty vmIP still matches by name",
			vmName:      "vm-web-01",
			vmIP:        "",
			pendingKeys: []string{"vm-web-01"},
			wantMinion:  "vm-web-01",
			wantMethod:  salt.MatchByName,
		},
		{
			name:        "empty vmName still matches by IP",
			vmName:      "",
			vmIP:        "10.0.0.5",
			pendingKeys: []string{"10.0.0.5"},
			wantMinion:  "10.0.0.5",
			wantMethod:  salt.MatchByIP,
		},
		{
			name:        "prefix does not match partial word (no dot)",
			vmName:      "vm-web",
			vmIP:        "",
			pendingKeys: []string{"vm-web-01", "vm-web-02"}, // no dot after vmName
			wantMinion:  "",
			wantMethod:  salt.MatchNone,
		},
		{
			name:        "case-insensitive name match — Windows uppercase minion",
			vmName:      "win-dc-01",
			vmIP:        "10.1.2.3",
			pendingKeys: []string{"WIN-DC-01"},
			wantMinion:  "WIN-DC-01",
			wantMethod:  salt.MatchByName,
		},
		{
			name:        "case-insensitive name match — mixed case",
			vmName:      "vm-web-01",
			vmIP:        "10.1.2.3",
			pendingKeys: []string{"VM-Web-01"},
			wantMinion:  "VM-Web-01",
			wantMethod:  salt.MatchByName,
		},
		{
			name:        "case-insensitive prefix match — Windows FQDN minion",
			vmName:      "win-dc-01",
			vmIP:        "10.1.2.3",
			pendingKeys: []string{"WIN-DC-01.corp.local"},
			wantMinion:  "WIN-DC-01.corp.local",
			wantMethod:  salt.MatchByPrefix,
		},
		{
			name:        "case-insensitive prefix match — mixed case FQDN",
			vmName:      "vm-db-02",
			vmIP:        "",
			pendingKeys: []string{"VM-DB-02.corp.local"},
			wantMinion:  "VM-DB-02.corp.local",
			wantMethod:  salt.MatchByPrefix,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := salt.FindMinionID(tt.vmName, tt.vmIP, tt.pendingKeys)
			if result.MinionID != tt.wantMinion {
				t.Errorf("MinionID: got %q, want %q", result.MinionID, tt.wantMinion)
			}
			if result.Method != tt.wantMethod {
				t.Errorf("Method: got %q, want %q", result.Method, tt.wantMethod)
			}
		})
	}
}

func TestShiftFirstOctet_EdgeCases(t *testing.T) {
	// Test the shiftFirstOctet logic indirectly via FindMinionID
	tests := []struct {
		ip        string
		pendingIP string // what the minion registered as
		wantMatch bool
	}{
		{"10.192.168.100", "192.168.100.10", true},
		{"10.0.0.1", "0.0.1.10", true},
		{"256.0.0.1", "0.0.1.256", false}, // invalid IP — but matcher won't crash
		{"not-an-ip", "not-an-ip", false},
		{"", "", false},
	}
	for _, tt := range tests {
		result := salt.FindMinionID("no-name-match", tt.ip, []string{tt.pendingIP})
		matched := result.Method == salt.MatchByShiftedIP
		if matched != tt.wantMatch {
			t.Errorf("shiftFirstOctet(%q) matching %q: got match=%v, want %v", tt.ip, tt.pendingIP, matched, tt.wantMatch)
		}
	}
}
