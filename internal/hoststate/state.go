// Package hoststate tracks ManagedService lifecycle state without depending on
// OpenTelemetry or any other third-party package.
package hoststate

import (
	"sort"
	"sync"
)

// State is a finite ManagedService lifecycle state.
type State string

// ManagedService lifecycle states. Their values are metric label values, so
// additions must remain finite and must not contain instance-specific data.
const (
	StateCreated    State = "created"
	StateStarting   State = "starting"
	StateRunning    State = "running"
	StateReady      State = "ready"
	StateFailed     State = "failed"
	StateDraining   State = "draining"
	StateStopping   State = "stopping"
	StateStopped    State = "stopped"
	StateNotStarted State = "not_started"
)

type key struct {
	module  string
	service string
	state   State
}

type counts struct {
	mu     sync.RWMutex
	values map[key]int64
}

var current = counts{values: make(map[key]int64)}

// Service is the stdlib-only bridge between one Host-managed service and the
// process-wide aggregate. Its identity is never included in metric labels.
type Service struct {
	module string
	name   string
	mu     sync.Mutex
	state  State
	failed bool
}

// NewService registers one ManagedService instance in the created state.
func NewService(module, name string) *Service {
	s := &Service{module: module, name: name, state: StateCreated}
	current.mu.Lock()
	current.values[key{module: module, service: name, state: StateCreated}]++
	current.mu.Unlock()
	return s
}

// Transition changes this instance's state once. Failed is sticky so cleanup
// attempts cannot hide the phase that caused the Host to roll back.
func (s *Service) Transition(next State) {
	if s == nil || !valid(next) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == next || (s.failed && next != StateFailed) {
		return
	}
	if next == StateFailed {
		s.failed = true
	}
	current.mu.Lock()
	from := key{module: s.module, service: s.name, state: s.state}
	if current.values[from] <= 1 {
		delete(current.values, from)
	} else {
		current.values[from]--
	}
	current.values[key{module: s.module, service: s.name, state: next}]++
	current.mu.Unlock()
	s.state = next
}

// Count is an aggregate snapshot row for one module, service, and state.
type Count struct {
	Module  string
	Service string
	State   State
	Count   int64
}

// Snapshot returns a stable copy of all positive process-wide state counts.
func Snapshot() []Count {
	current.mu.RLock()
	out := make([]Count, 0, len(current.values))
	for k, count := range current.values {
		if count > 0 {
			out = append(out, Count{Module: k.module, Service: k.service, State: k.state, Count: count})
		}
	}
	current.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Module != out[j].Module {
			return out[i].Module < out[j].Module
		}
		if out[i].Service != out[j].Service {
			return out[i].Service < out[j].Service
		}
		return out[i].State < out[j].State
	})
	return out
}

func valid(state State) bool {
	switch state {
	case StateCreated, StateStarting, StateRunning, StateReady, StateFailed,
		StateDraining, StateStopping, StateStopped, StateNotStarted:
		return true
	default:
		return false
	}
}
