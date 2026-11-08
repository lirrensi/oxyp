// Package peerfederation mounts remote oxyp daemons as local servers.
//
// One [peers.<name>] entry expands to N virtual servers keyed
// "peer:server". Discovery metadata is fetched over the peer's native REST
// API and cached like remote metadata; invocations proxy to the owner, which
// re-runs its own gates. Auth never travels: the owner holds its tokens,
// the proxy holds only the peer's bearer token.
package peerfederation
