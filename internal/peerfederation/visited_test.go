package peerfederation

import (
	"context"
	"testing"
)

func TestVisitedChain(t *testing.T) {
	if got := ParseVisited("a, b,,c "); len(got) != 3 || got[1] != "b" {
		t.Fatalf("parse = %v", got)
	}
	ctx := WithVisited(context.Background(), []string{"a", "b"})
	ctx = WithInstance(ctx, "self")
	if got := VisitedHeader(ctx); got != "a,b,self" {
		t.Fatalf("header = %q", got)
	}
	if id, ok := InstanceFrom(ctx); !ok || id != "self" {
		t.Fatalf("instance = %q %v", id, ok)
	}
	if !CheckLoop("self", []string{"a", "self"}) {
		t.Fatal("self in chain must loop")
	}
	if CheckLoop("self", []string{"a"}) {
		t.Fatal("clean chain must pass")
	}
	long := make([]string, MaxVisited)
	for i := range long {
		long[i] = "x"
	}
	if !CheckLoop("self", long) {
		t.Fatal("full chain must loop")
	}
	if CheckOutbound("self", "tgt", []string{"self"}) {
		t.Fatal("self in outbound chain must fail")
	}
	if CheckOutbound("self", "tgt", []string{"tgt"}) {
		t.Fatal("target revisit must fail")
	}
	if !CheckOutbound("self", "tgt", []string{"other"}) {
		t.Fatal("fresh hop must pass")
	}
	a, b := NewInstanceID(), NewInstanceID()
	if a == "" || a == b || len(a) != 36 {
		t.Fatalf("instance ids must be unique UUIDs: %q %q", a, b)
	}
}
