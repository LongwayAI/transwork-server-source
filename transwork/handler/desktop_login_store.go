package handler

import (
	"sync"
	"time"
)

type loginStateEntry struct {
	LoopbackURL string
	ExpiresAt   time.Time
}

type bootstrapEntry struct {
	UserID    int
	IdToken   string // OIDC id_token captured at code-exchange time; needed later for RP-initiated logout (id_token_hint).
	ExpiresAt time.Time
}

type loginStateStore struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]loginStateEntry
}

func newLoginStateStore(ttl time.Duration) *loginStateStore {
	return &loginStateStore{ttl: ttl, m: make(map[string]loginStateEntry)}
}

func (s *loginStateStore) put(state string, entry loginStateEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry.ExpiresAt = time.Now().Add(s.ttl)
	s.m[state] = entry
	s.gcLocked()
}

func (s *loginStateStore) consume(state string) (loginStateEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.m[state]
	if !ok {
		return loginStateEntry{}, false
	}
	delete(s.m, state)
	if time.Now().After(entry.ExpiresAt) {
		return loginStateEntry{}, false
	}
	return entry, true
}

func (s *loginStateStore) gcLocked() {
	now := time.Now()
	for k, v := range s.m {
		if now.After(v.ExpiresAt) {
			delete(s.m, k)
		}
	}
}

type bootstrapCodeStore struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]bootstrapEntry
}

func newBootstrapCodeStore(ttl time.Duration) *bootstrapCodeStore {
	return &bootstrapCodeStore{ttl: ttl, m: make(map[string]bootstrapEntry)}
}

func (s *bootstrapCodeStore) put(code string, entry bootstrapEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry.ExpiresAt = time.Now().Add(s.ttl)
	s.m[code] = entry
	s.gcLocked()
}

func (s *bootstrapCodeStore) gcLocked() {
	now := time.Now()
	for k, v := range s.m {
		if now.After(v.ExpiresAt) {
			delete(s.m, k)
		}
	}
}

func (s *bootstrapCodeStore) consume(code string) (bootstrapEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.m[code]
	if !ok {
		return bootstrapEntry{}, false
	}
	delete(s.m, code)
	if time.Now().After(entry.ExpiresAt) {
		return bootstrapEntry{}, false
	}
	return entry, true
}

// idTokenByTokenID holds the OIDC id_token associated with each active
// desktop-login token row, keyed by Token.Id. Populated during exchange and
// consumed by AuthLogout to build the IdP end-session URL. In-memory only:
// a server restart drops the map, which means the post-restart logout cannot
// end the IdP session. That is acceptable because `prompt=login` on
// the authorize URL still lets the user pick a different account on next
// sign-in (the user-visible symptom this whole feature targets).
var idTokenByTokenID sync.Map // map[int]string

// Package-level singletons used by handlers in the same package.
var (
	desktopLoginStates = newLoginStateStore(5 * time.Minute)
	bootstrapCodes     = newBootstrapCodeStore(5 * time.Minute)
)
