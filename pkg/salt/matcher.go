package salt

import (
	"fmt"
	"strconv"
	"strings"
)

// MatchMethod describes which matching strategy found the minion ID.
type MatchMethod string

const (
	MatchByName      MatchMethod = "name"
	MatchByIP        MatchMethod = "ip"
	MatchByShiftedIP MatchMethod = "shifted-ip"
	MatchByPrefix    MatchMethod = "prefix"
	MatchNone        MatchMethod = "none"
)

// MatchResult holds the outcome of a minion ID lookup.
type MatchResult struct {
	MinionID string
	Method   MatchMethod
}

// FindMinionID searches a list of pending Salt key IDs for one that corresponds to a given VM.
//
// Matching is attempted in priority order:
//  1. Exact name match  — EqualFold(vmName, key) (case-insensitive, covers Windows uppercase hostnames)
//  2. IP match          — vmIP == key (DNS not ready at minion first boot)
//  3. Shifted IP        — first octet of IP rotated to last position (edge case from existing env)
//  4. Name prefix       — case-insensitive HasPrefix (FQDN minion IDs like "WIN-01.corp.local")
//
// Returns MatchNone if no pending key matches.
func FindMinionID(vmName, vmIP string, pendingKeys []string) MatchResult {
	shiftedIP := shiftFirstOctet(vmIP)

	for _, key := range pendingKeys {
		// 1. Exact name match (case-insensitive: Windows minions register with uppercase hostnames)
		if strings.EqualFold(key, vmName) {
			return MatchResult{MinionID: key, Method: MatchByName}
		}
	}

	for _, key := range pendingKeys {
		// 2. IP match
		if vmIP != "" && key == vmIP {
			return MatchResult{MinionID: key, Method: MatchByIP}
		}
		// 3. Shifted IP match
		if shiftedIP != "" && key == shiftedIP {
			return MatchResult{MinionID: key, Method: MatchByShiftedIP}
		}
	}

	for _, key := range pendingKeys {
		// 4. Name prefix (e.g. key="WIN-DC-01.corp.local", vmName="win-dc-01") — case-insensitive
		if vmName != "" && strings.HasPrefix(strings.ToLower(key), strings.ToLower(vmName)+".") {
			return MatchResult{MinionID: key, Method: MatchByPrefix}
		}
	}

	return MatchResult{Method: MatchNone}
}

// shiftFirstOctet takes an IPv4 string and returns a version with the first octet moved to last.
// "10.192.168.100" → "192.168.100.10"
// This handles the edge case seen in the existing vcenter_cleanup.py and vcfa_supervisor.py
// scripts where some Salt minions generate their ID from an IP with the first octet rotated.
// Returns empty string if the input is not a valid IPv4.
func shiftFirstOctet(ip string) string {
	if ip == "" {
		return ""
	}
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		return ""
	}
	for _, p := range parts {
		if len(p) == 0 || len(p) > 3 {
			return ""
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 {
			return ""
		}
	}
	return fmt.Sprintf("%s.%s.%s.%s", parts[1], parts[2], parts[3], parts[0])
}
