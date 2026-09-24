package hoststate

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

var testSequence atomic.Uint64

func testIdentity() (string, string) {
	id := testSequence.Add(1)
	return fmt.Sprintf("hoststate-test-%d", id), "managed"
}

func stateCounts(module, service string) map[State]int64 {
	got := make(map[State]int64)
	for _, row := range Snapshot() {
		if row.Module == module && row.Service == service {
			got[row.State] = row.Count
		}
	}
	return got
}

func TestServiceTransitionsAreIdempotentAndAggregateByStableIdentity(t *testing.T) {
	module, name := testIdentity()
	first := NewService(module, name)
	second := NewService(module, name)
	if got := stateCounts(module, name); got[StateCreated] != 2 || len(got) != 1 {
		t.Fatalf("initial aggregate = %v, want created=2", got)
	}

	first.Transition(StateReady)
	first.Transition(StateReady)
	second.Transition(StateFailed)
	second.Transition(StateStopping) // failure remains the visible terminal cause
	if got := stateCounts(module, name); got[StateReady] != 1 || got[StateFailed] != 1 || len(got) != 2 {
		t.Fatalf("aggregate after repeated/failing transitions = %v, want ready=1 failed=1", got)
	}
	first.Transition(StateDraining)
	first.Transition(StateStopping)
	first.Transition(StateStopped)
	if got := stateCounts(module, name); got[StateStopped] != 1 || got[StateFailed] != 1 || len(got) != 2 {
		t.Fatalf("aggregate after normal cleanup = %v, want stopped=1 failed=1", got)
	}
	first.Transition(State("unbounded-user-value"))
	if got := stateCounts(module, name); got[StateStopped] != 1 || len(got) != 2 {
		t.Fatalf("invalid state changed aggregate: %v", got)
	}
}

func TestConcurrentInstancesKeepAggregateCountsExact(t *testing.T) {
	module, name := testIdentity()
	const instances = 128
	services := make([]*Service, instances)
	var create sync.WaitGroup
	create.Add(instances)
	for i := range services {
		go func(i int) {
			defer create.Done()
			services[i] = NewService(module, name)
		}(i)
	}
	create.Wait()
	if got := stateCounts(module, name); got[StateCreated] != instances || len(got) != 1 {
		t.Fatalf("created aggregate = %v, want created=%d", got, instances)
	}

	var transitions sync.WaitGroup
	transitions.Add(instances)
	for _, service := range services {
		go func(service *Service) {
			defer transitions.Done()
			service.Transition(StateRunning)
			service.Transition(StateReady)
			service.Transition(StateReady)
		}(service)
	}
	transitions.Wait()
	if got := stateCounts(module, name); got[StateReady] != instances || len(got) != 1 {
		t.Fatalf("ready aggregate = %v, want ready=%d", got, instances)
	}
}

func TestSnapshotIsSortedByModuleServiceAndState(t *testing.T) {
	module, name := testIdentity()
	NewService(module+"-z", name).Transition(StateReady)
	NewService(module+"-a", name+"-z").Transition(StateReady)
	NewService(module+"-a", name+"-a").Transition(StateReady)

	var rows []Count
	for _, row := range Snapshot() {
		if row.Module == module+"-z" || row.Module == module+"-a" {
			rows = append(rows, row)
		}
	}
	if len(rows) != 3 {
		t.Fatalf("snapshot rows = %v, want 3", rows)
	}
	if rows[0].Module != module+"-a" || rows[0].Service != name+"-a" ||
		rows[1].Module != module+"-a" || rows[1].Service != name+"-z" ||
		rows[2].Module != module+"-z" {
		t.Fatalf("snapshot is not sorted: %v", rows)
	}
}
