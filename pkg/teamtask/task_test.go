package teamtask

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestMemStore_CreateListGet(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	id, err := s.Create(ctx, Task{Title: "research"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := s.Get(ctx, id)
	if err != nil || got.Title != "research" || got.State != TaskPending {
		t.Fatalf("get: %+v err=%v", got, err)
	}
	if _, err := s.Get(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing task: want ErrNotFound, got %v", err)
	}
	list, _ := s.List(ctx)
	if len(list) != 1 {
		t.Fatalf("list: want 1, got %d", len(list))
	}
}

func TestMemStore_ClaimCompleteLifecycle(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	id, _ := s.Create(ctx, Task{Title: "t"})

	claimed, err := s.Claim(ctx, id, "alice")
	if err != nil || claimed.State != TaskInProgress || claimed.Owner != "alice" {
		t.Fatalf("claim: %+v err=%v", claimed, err)
	}
	// A second claim must fail — no two members win the same task.
	if _, err := s.Claim(ctx, id, "bob"); !errors.Is(err, ErrNotClaimable) {
		t.Fatalf("double claim: want ErrNotClaimable, got %v", err)
	}
	// A non-owner cannot complete.
	if err := s.Complete(ctx, id, "bob", "x"); !errors.Is(err, ErrNotOwner) {
		t.Fatalf("non-owner complete: want ErrNotOwner, got %v", err)
	}
	if err := s.Complete(ctx, id, "alice", "done"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	done, _ := s.Get(ctx, id)
	if done.State != TaskCompleted || done.Result != "done" {
		t.Fatalf("completed task wrong: %+v", done)
	}
}

func TestMemStore_DependencyUnblock(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	a, _ := s.Create(ctx, Task{Title: "a"})
	b, _ := s.Create(ctx, Task{Title: "b", Deps: []string{a}})

	// b is blocked until a is completed.
	if _, err := s.Claim(ctx, b, "m"); !errors.Is(err, ErrNotClaimable) {
		t.Fatalf("blocked task: want ErrNotClaimable, got %v", err)
	}
	if _, err := s.Claim(ctx, a, "m"); err != nil {
		t.Fatalf("claim a: %v", err)
	}
	// Still blocked while a is only inProgress.
	if _, err := s.Claim(ctx, b, "m"); !errors.Is(err, ErrNotClaimable) {
		t.Fatalf("dep inProgress: b must stay blocked, got %v", err)
	}
	if err := s.Complete(ctx, a, "m", "ok"); err != nil {
		t.Fatalf("complete a: %v", err)
	}
	// Now b is claimable.
	if _, err := s.Claim(ctx, b, "m"); err != nil {
		t.Fatalf("after dep complete, b must be claimable: %v", err)
	}
}

func TestMemStore_DanglingDepBlocks(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	b, _ := s.Create(ctx, Task{Title: "b", Deps: []string{"ghost"}})
	if _, err := s.Claim(ctx, b, "m"); !errors.Is(err, ErrNotClaimable) {
		t.Fatalf("dangling dep must block (fail-closed), got %v", err)
	}
}

// TestMemStore_ConcurrentClaim asserts exactly one of N racing claimers wins.
func TestMemStore_ConcurrentClaim(t *testing.T) {
	ctx := context.Background()
	s := NewMemStore()
	id, _ := s.Create(ctx, Task{Title: "hot"})

	const racers = 20
	var wins int
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func() {
			defer wg.Done()
			if _, err := s.Claim(ctx, id, "racer"); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("exactly one claimer must win, got %d", wins)
	}
}

func TestBucketName(t *testing.T) {
	if got := BucketName("tenant-a", "researchers"); got != "team_tenant-a_researchers" {
		t.Fatalf("bucket name: got %q", got)
	}
}
