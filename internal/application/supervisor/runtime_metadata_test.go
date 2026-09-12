package supervisor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ishii1648/codex-issue-loop/internal/adapter/state"
	"github.com/ishii1648/codex-issue-loop/internal/adapter/worker"
	"github.com/ishii1648/codex-issue-loop/internal/platform/runtimemetadata"
)

func TestRuntimeMetadataLivesWithinSupervisorLock(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(map[bool]string{false: "published", true: "publish_failure"}[unavailable], func(t *testing.T) {
			loop, _ := testLoop(t, worker.Result{})
			root := t.TempDir()
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			loop.OperatorMaintenanceFencePath = filepath.Join(root, "maintenance.json")
			if err := os.WriteFile(loop.OperatorMaintenanceFencePath, []byte(`{"version":1}`), 0600); err != nil {
				t.Fatal(err)
			}
			started := time.Now().Add(-time.Minute).Unix()
			loop.ReleaseVersion = "1.8.2"
			loop.RuntimeMetadata = runtimemetadata.Store{Root: root, Inspect: func(pid int) (runtimemetadata.Process, error) {
				if lock, err := loop.Store.AcquireSupervisorLock(); err == nil {
					state.ReleaseSupervisorLock(lock)
					t.Error("metadata published without supervisor lock")
				}
				if unavailable {
					return runtimemetadata.Process{}, os.ErrPermission
				}
				return runtimemetadata.Process{PID: pid, UID: uint32(os.Geteuid()), BootSessionID: "boot", StartedAt: runtimemetadata.StartTime{Seconds: started}}, nil
			}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- loop.Run(ctx) }()
			deadline := time.Now().Add(3 * time.Second)
			for {
				snapshot, err := loop.Store.Load()
				if err == nil && snapshot.Supervisor.State == state.SupervisorStateMaintenance {
					break
				}
				if time.Now().After(deadline) {
					cancel()
					<-done
					t.Fatal("supervisor did not enter maintenance")
				}
				time.Sleep(time.Millisecond)
			}
			observation := loop.RuntimeMetadata.Observe(loop.Config.GitHub.Repo, time.Now(), time.Minute)
			if unavailable && observation != nil || !unavailable && (observation == nil || observation.Version != "1.8.2") {
				t.Fatalf("runtime=%+v", observation)
			}
			cancel()
			if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			files, err := filepath.Glob(filepath.Join(root, "runtime-metadata", "*", "*.json"))
			if err != nil || len(files) != 0 {
				t.Fatalf("cleanup files=%v err=%v", files, err)
			}
			lock, err := loop.Store.AcquireSupervisorLock()
			if err != nil {
				t.Fatal(err)
			}
			state.ReleaseSupervisorLock(lock)
		})
	}
}
