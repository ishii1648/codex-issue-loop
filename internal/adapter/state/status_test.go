package state

import (
	"encoding/json"
	issuedomain "github.com/ishii1648/codex-issue-loop/internal/domain/issue"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestStatusReadCommittedReadOnlyAndRecovery(t *testing.T) {
	for _, scenario := range []string{"writer", "transaction", "recovery transaction", "unknown version", "invalid", "missing lock", "history"} {
		t.Run(scenario, func(t *testing.T) {
			store := newStore(t)
			snapshot, err := store.Update("fixture", 0, "", nil, func(*Snapshot) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(store.StatePath())
			if err != nil {
				t.Fatal(err)
			}
			want := ""
			restore := func() {}
			write := func(path string, data []byte) {
				t.Helper()
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "writer":
				lock, err := store.lock(true)
				if err != nil {
					t.Fatal(err)
				}
				restore = func() { unlock(lock) }
				want = "state_busy"
			case "transaction", "recovery transaction":
				path := store.TransactionPath()
				if scenario == "recovery transaction" {
					path = store.quarantineRecoveryTransactionPath()
				}
				write(path, []byte("{"))
				restore = func() {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				}
				want = "state_unconfirmed"
			case "unknown version":
				write(store.StatePath(), []byte(`{"version":999,"issues":"future format"}`))
				want = "unsupported_snapshot_version"
			case "invalid":
				snapshot.Supervisor.State = "unknown"
				data, err := json.Marshal(snapshot)
				if err != nil {
					t.Fatal(err)
				}
				write(store.StatePath(), data)
				want = "invalid_state"
			case "missing lock":
				if err := os.Remove(store.lockPath()); err != nil {
					t.Fatal(err)
				}
				restore = func() { write(store.lockPath(), nil) }
				want = "state_unavailable"
			case "history":
				write(store.EventsPath(), []byte("invalid event history"))
			}
			before := diagnosticFiles(t, store.Dir)
			got, err := store.ReadStatusSnapshot()
			if want == "" {
				if err != nil || got.StateRevision != snapshot.StateRevision {
					t.Fatalf("snapshot=%+v err=%v", got, err)
				}
			} else if err == nil || err.Error() != want {
				t.Fatalf("want %s got %v", want, err)
			}
			if !reflect.DeepEqual(before, diagnosticFiles(t, store.Dir)) {
				t.Fatal("read changed files")
			}
			restore()
			write(store.StatePath(), original)
			got, err = store.ReadStatusSnapshot()
			if err != nil || got.StateRevision != snapshot.StateRevision {
				t.Fatalf("no-progress recovery: %v", err)
			}
		})
	}
}

func TestStatusConcurrentUpdatesAreSingleRevision(t *testing.T) {
	store := newStore(t)
	initial, err := store.Update("fixture", 0, "", nil, func(*Snapshot) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		for n := 0; n < 40; n++ {
			_, err := store.Update("fixture", 0, "", nil, func(s *Snapshot) error {
				s.Supervisor.ConsecutiveFailures = int(s.StateRevision + 1 - initial.StateRevision)
				return nil
			})
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for n := 0; n < 80; n++ {
		s, err := store.ReadStatusSnapshot()
		if err != nil {
			if err.Error() != "state_busy" {
				t.Fatal(err)
			}
			continue
		}
		if int(s.StateRevision-initial.StateRevision) != s.Supervisor.ConsecutiveFailures {
			t.Fatalf("mixed revision: %+v", s)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	s, err := store.ReadStatusSnapshot()
	if err != nil || s.StateRevision != initial.StateRevision+40 || s.Supervisor.ConsecutiveFailures != 40 {
		t.Fatalf("latest update missing: %+v %v", s, err)
	}
}

func TestStatusReflectsFormalAnswerAndCancellation(t *testing.T) {
	store := newStore(t)
	now := time.Now().UTC()
	_, request, err := store.AskIntake(1, "scope", "which scope?", "scope required", "small", nil, true, now)
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.ReadStatusSnapshot()
	if err != nil || !before.NeedsHuman(1, true) {
		t.Fatalf("before answer: %v", err)
	}
	external := store
	answered, _, err := external.RecordAnswer(request.ID, "small", now)
	if err != nil {
		t.Fatal(err)
	}
	after, err := store.ReadStatusSnapshot()
	if err != nil || after.StateRevision != answered.StateRevision || after.NeedsHuman(1, true) {
		t.Fatalf("answer not visible: %v", err)
	}
	blocked, err := external.Update("fixture", 2, "run_2", nil, func(s *Snapshot) error {
		s.Issues["2"] = &Issue{Number: 2, Status: issuedomain.StatusBlocked, RunID: "run_2", GitHubStateReason: "NOT_PLANNED", UpdatedAt: now}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	canceled, err := external.Update("issue_canceled", 2, "run_2", nil, func(s *Snapshot) error {
		_, err := ApplyNotPlannedCancellation(s, 2, blocked.Issues["2"], now)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	after, err = store.ReadStatusSnapshot()
	if err != nil || after.StateRevision != canceled.StateRevision || after.Issues["2"].Status != issuedomain.StatusCanceled {
		t.Fatalf("cancel not visible: %v", err)
	}
}
