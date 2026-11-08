package peerfederation

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// visitedCtxKey carries the inbound federation chain (instance ids).
type visitedCtxKey struct{}

// instanceCtxKey carries this daemon's own instance id for outbound calls.
type instanceCtxKey struct{}

// NewInstanceID mints a random daemon id (UUIDv4 string).
func NewInstanceID() string {
	return uuid.NewString()
}

// WithInstance returns ctx carrying this daemon's id for outbound peer calls.
func WithInstance(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, instanceCtxKey{}, id)
}

// InstanceFrom reads the outbound instance id from ctx.
func InstanceFrom(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(instanceCtxKey{}).(string)
	return v, ok && v != ""
}

// WithVisited returns ctx carrying the inbound visited chain. Entries are
// raw instance ids; over-long or empty entries are dropped.
func WithVisited(ctx context.Context, chain []string) context.Context {
	out := make([]string, 0, len(chain))
	for _, id := range chain {
		if id = strings.TrimSpace(id); id != "" && len(out) < MaxVisited {
			out = append(out, id)
		}
	}
	if len(out) == 0 {
		return ctx
	}
	return context.WithValue(ctx, visitedCtxKey{}, out)
}

// VisitedFrom reads the inbound visited chain from ctx.
func VisitedFrom(ctx context.Context) []string {
	v, _ := ctx.Value(visitedCtxKey{}).([]string)
	return v
}

// ParseVisited splits an inbound HeaderVisited value into ids.
func ParseVisited(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	var out []string
	for _, id := range strings.Split(v, ",") {
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// VisitedHeader renders the outbound chain: inbound chain + self id.
// Empty when nothing is known (direct local call, no federation yet).
func VisitedHeader(ctx context.Context) string {
	chain := VisitedFrom(ctx)
	self, _ := InstanceFrom(ctx)
	if self != "" && !contains(chain, self) {
		chain = append(append([]string(nil), chain...), self)
	}
	return strings.Join(chain, ",")
}

// CheckLoop reports whether serving this request would loop: self already
// in the inbound chain, or the chain at the hop cap.
func CheckLoop(self string, chain []string) bool {
	if self != "" && contains(chain, self) {
		return true
	}
	return len(chain) >= MaxVisited
}

// CheckOutbound reports whether dialing a peer with the given chain is safe:
// the chain must not already contain self or the target instance.
func CheckOutbound(self, target string, chain []string) bool {
	if self != "" && contains(chain, self) {
		return false
	}
	if target != "" && contains(chain, target) {
		return false
	}
	return len(chain) < MaxVisited
}

func contains(chain []string, id string) bool {
	for _, c := range chain {
		if c == id {
			return true
		}
	}
	return false
}

// InboundChain extracts the visited chain from an incoming request header.
func InboundChain(r *http.Request) []string {
	return ParseVisited(r.Header.Get(HeaderVisited))
}

// peerCtxKey marks ctx as carrying an inbound peer request.
type peerCtxKey struct{}

// WithPeer returns ctx marked as an inbound peer (federation) request.
func WithPeer(ctx context.Context) context.Context {
	return context.WithValue(ctx, peerCtxKey{}, true)
}

// IsPeer reports whether ctx carries an inbound peer request.
func IsPeer(ctx context.Context) bool {
	v, _ := ctx.Value(peerCtxKey{}).(bool)
	return v
}

// IsPeerRequest reports whether an incoming HTTP request is a federation
// call from a peer daemon (carries the instance header our client always
// sends). Local CLI/shim/MCP clients never send it.
func IsPeerRequest(r *http.Request) bool {
	return strings.TrimSpace(r.Header.Get(HeaderInstance)) != ""
}
