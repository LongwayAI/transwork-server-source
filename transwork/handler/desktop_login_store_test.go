package handler

import (
	"testing"
	"time"
)

func TestLoginStateStorePutAndConsume(t *testing.T) {
	store := newLoginStateStore(time.Minute)
	state := "state-123"
	store.put(state, loginStateEntry{LoopbackURL: "http://127.0.0.1:5511/cb"})

	got, ok := store.consume(state)
	if !ok {
		t.Fatalf("expected to consume state once")
	}
	if got.LoopbackURL != "http://127.0.0.1:5511/cb" {
		t.Fatalf("unexpected loopback url: %q", got.LoopbackURL)
	}

	if _, ok := store.consume(state); ok {
		t.Fatalf("state must be single-use")
	}
}

func TestLoginStateStoreExpires(t *testing.T) {
	store := newLoginStateStore(10 * time.Millisecond)
	store.put("s", loginStateEntry{LoopbackURL: "http://127.0.0.1:1/cb"})
	time.Sleep(20 * time.Millisecond)
	if _, ok := store.consume("s"); ok {
		t.Fatalf("expected expiry")
	}
}

func TestBootstrapCodeStorePutAndConsume(t *testing.T) {
	store := newBootstrapCodeStore(time.Minute)
	store.put("code-1", bootstrapEntry{UserID: 42})
	got, ok := store.consume("code-1")
	if !ok || got.UserID != 42 {
		t.Fatalf("expected userID 42, got %+v ok=%v", got, ok)
	}
	if _, ok := store.consume("code-1"); ok {
		t.Fatalf("code must be single-use")
	}
}

func TestBootstrapCodeStoreExpires(t *testing.T) {
	store := newBootstrapCodeStore(10 * time.Millisecond)
	store.put("c", bootstrapEntry{UserID: 7})
	time.Sleep(20 * time.Millisecond)
	if _, ok := store.consume("c"); ok {
		t.Fatalf("expected expiry")
	}
}
