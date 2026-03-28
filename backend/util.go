package backend

// IsAllowed reports whether senderID is permitted by the allowlist.
// An empty or nil allowlist permits all senders.
func IsAllowed(allowlist map[string]struct{}, senderID string) bool {
	if len(allowlist) == 0 {
		return true
	}

	_, ok := allowlist[senderID]

	return ok
}
