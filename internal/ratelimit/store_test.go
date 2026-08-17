package ratelimit

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestStoreSharesAndAtomicallyCountsSuppressedRetries(t *testing.T) {
	now := time.Date(2026, 8, 17, 2, 0, 0, 0, time.UTC)
	store := Store{Path: filepath.Join(t.TempDir(), "github-rate-limit.json")}
	want := Cooldown{Resource: "graphql", ResetAt: now.Add(time.Hour), Source: "rest-rate-limit"}
	if _, err := store.Observe(want, now); err != nil {
		t.Fatal(err)
	}
	const supervisors = 12
	var group sync.WaitGroup
	for range supervisors {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, active, err := store.Suppress(now); err != nil || !active {
				t.Errorf("active=%v err=%v", active, err)
			}
		}()
	}
	group.Wait()
	got, active, err := store.Current(now)
	if err != nil {
		t.Fatal(err)
	}
	if !active || !got.ResetAt.Equal(want.ResetAt) || got.SuppressedRetryCount != supervisors {
		t.Fatalf("cooldown=%+v active=%v", got, active)
	}
}

func TestStoreNeverShortensActiveCooldown(t *testing.T) {
	now := time.Date(2026, 8, 17, 2, 0, 0, 0, time.UTC)
	store := Store{Path: filepath.Join(t.TempDir(), "github-rate-limit.json")}
	later := Cooldown{Resource: "graphql", ResetAt: now.Add(time.Hour), Source: "rest-rate-limit"}
	if _, err := store.Observe(later, now); err != nil {
		t.Fatal(err)
	}
	got, err := store.Observe(Cooldown{Resource: "graphql", ResetAt: now.Add(time.Minute), Source: "fallback"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !got.ResetAt.Equal(later.ResetAt) || got.Source != later.Source {
		t.Fatalf("active cooldown was shortened: %+v", got)
	}
}
