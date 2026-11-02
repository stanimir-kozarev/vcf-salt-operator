package salt

import (
	"fmt"
	"strconv"
	"strings"
)

type MatchMethod string

const (
	MatchByName      MatchMethod = "name"
	MatchByIP        MatchMethod = "ip"
	MatchByShiftedIP MatchMethod = "shifted-ip"
	MatchByPrefix    MatchMethod = "prefix"
	MatchNone        MatchMethod = "none"
)

type MatchResult struct {
	MinionID string
	Method   MatchMethod
}

func FindMinionID(vmName, vmIP string, pendingKeys []string) MatchResult {
	shiftedIP := shiftFirstOctet(vmIP)

	for _, key := range pendingKeys {
		if strings.EqualFold(key, vmName) {
			return MatchResult{MinionID: key, Method: MatchByName}
		}
	}

	for _, key := range pendingKeys {
		if vmIP != "" && key == vmIP {
			return MatchResult{MinionID: key, Method: MatchByIP}
		}
		if shiftedIP != "" && key == shiftedIP {
			return MatchResult{MinionID: key, Method: MatchByShiftedIP}
		}
	}

	for _, key := range pendingKeys {
		if vmName != "" && strings.HasPrefix(strings.ToLower(key), strings.ToLower(vmName)+".") {
			return MatchResult{MinionID: key, Method: MatchByPrefix}
		}
	}

	return MatchResult{Method: MatchNone}
}

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
