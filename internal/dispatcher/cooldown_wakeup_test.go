package dispatcher

import (
	"context"
	"testing"
	"time"

	"pwnmesh/internal/board"
	"pwnmesh/internal/worker"
)

func TestRunWakesAtWorkerCooldownWithoutWaitingForTick(t *testing.T) {
	for _, failure := range []string{"unhealthy", "rejected", "recovery"} {
		t.Run(failure, func(t *testing.T) {
			scheduler, runner, graph, _ := wakeupFixture(t, 1)
			runner.waitAll = true
			backend := scheduler.Config.Workers[0]
			var original worker.Job
			if failure == "recovery" {
				run := &task{Job: worker.Job{RunID: "interrupted", Kind: "reason", Graph: graph, GraphRPC: true, ResultContractVersion: 2, Workspace: "/workspace", EnvironmentID: scheduler.environmentID(backend), Budget: scheduler.Config.Task("reason"), DecisionTrigger: "initial"}, Worker: backend, Lease: Lease{Run: backend.Name + "@interrupted", Kind: "reason"}}
				if err := scheduler.Client.Do(context.Background(), "POST", projectPath(graph.Project.ID)+"/reason/claim", map[string]string{"worker": run.Lease.Run, "trigger": "initial"}, nil, nil); err != nil {
					t.Fatal(err)
				}
				if err := scheduler.register(context.Background(), run); err != nil {
					t.Fatal(err)
				}
				original = run.Job
			}
			until := time.Now().Add(150 * time.Millisecond)
			if failure == "rejected" {
				scheduler.rejected[scheduler.rejectKey(graph.Project.ID, "reason", backend.Name)] = until
			} else {
				scheduler.unhealthy[backend.Name] = until
			}
			stop := runWakeupScheduler(t, scheduler)
			job := nextWakeupJob(t, runner.started)
			if time.Now().Before(until) {
				t.Fatal("worker started before its cooldown expired")
			}
			if job.Kind != "reason" || failure == "recovery" && digest(job) != digest(original) {
				t.Fatal("cooldown wake changed the registered execution or role")
			}
			stop()
		})
	}
}

func TestWorkerCooldownUsesLatestGateAndSkipsBusyBackends(t *testing.T) {
	scheduler, _, graph, _ := wakeupFixture(t, 1)
	backend := scheduler.Config.Workers[0]
	now := time.Now()
	healthy := now.Add(time.Second)
	accepted := now.Add(2 * time.Second)
	scheduler.unhealthy[backend.Name] = healthy
	scheduler.rejected[scheduler.rejectKey(graph.Project.ID, "reason", backend.Name)] = accepted
	if scheduler.choose(graph.Project.ID, "reason") != nil || !scheduler.nextWake.Equal(accepted) {
		t.Fatalf("both gates must expire before retry: next=%s", scheduler.nextWake)
	}
	scheduler.nextWake = time.Time{}
	for index := range backend.MaxRunning {
		id := string(rune('a' + index))
		scheduler.running[id] = &task{Worker: backend, Job: worker.Job{Kind: "reason", Graph: board.Graph{Project: graph.Project}}}
	}
	if scheduler.choose(graph.Project.ID, "reason") != nil || !scheduler.nextWake.IsZero() {
		t.Fatal("capacity-bound work should wait for completion, not a cooldown timer")
	}
	clear(scheduler.running)
	scheduler.unhealthy[backend.Name] = now.Add(-time.Second)
	scheduler.rejected[scheduler.rejectKey(graph.Project.ID, "reason", backend.Name)] = now.Add(-time.Second)
	if scheduler.choose(graph.Project.ID, "reason") == nil || !scheduler.nextWake.IsZero() {
		t.Fatal("expired cooldown created an immediate retry loop")
	}
}
