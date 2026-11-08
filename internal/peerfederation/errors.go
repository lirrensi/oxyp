package peerfederation

import "fmt"

// AuthNeededError signals the owner side needs an interactive login.
// The login must run ON THE OWNER (auth never travels); Hint is the
// owner-side "run: oxyp login <server>" text.
type AuthNeededError struct {
	Peer   string
	Server string
	Hint   string
}

func (e *AuthNeededError) Error() string {
	return fmt.Sprintf("peer %q server %q needs login on the owner: %s (on %s)", e.Peer, e.Server, e.Hint, e.Peer)
}

// PolicyDeniedError mirrors an owner-side tools-policy deny (403 blocked).
type PolicyDeniedError struct {
	Server  string
	Tool    string
	Message string
}

func (e *PolicyDeniedError) Error() string {
	if e.Tool != "" {
		return fmt.Sprintf("peer policy denied %s tool %q: %s", e.Server, e.Tool, e.Message)
	}
	return fmt.Sprintf("peer policy denied %s: %s", e.Server, e.Message)
}

// ParamDeniedError mirrors an owner-side params deny (403 param-blocked).
type ParamDeniedError struct {
	Server  string
	Tool    string
	Allowed []string
	Message string
}

func (e *ParamDeniedError) Error() string {
	return fmt.Sprintf("peer params denied %s tool %q (allowed: %v)", e.Server, e.Tool, e.Allowed)
}
