package web

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/provider"
	"github.com/kivali-ai/kivali/internal/store"
)

// Per-task model and effort are the main cost lever an agent has: a
// grep-shaped lookup on Haiku at low effort against a judgement call on
// Opus at high effort is a large multiple in spend, and only the agent
// dispatching knows which it is. These prove the choice survives the
// trip to the spawned process on BOTH paths — the durable agent's async
// dispatch and a sub-lead's blocking one, which take different routes
// through the service.

// modelEffortService returns a service whose driver hands every spawn
// request back on a channel, so tests wait on the call rather than on
// the clock.
func modelEffortService(t *testing.T) (*SubagentService, chan fakeDriveCall) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	calls := make(chan fakeDriveCall, 8)
	driver := &fakeSubagentDriver{
		respond: func(c fakeDriveCall) (string, error) {
			calls <- c
			return "ok", nil
		},
	}
	return &SubagentService{
		Provider:        provider.MockProvider{},
		Store:           st,
		Driver:          driver,
		DeliverToParent: func(string, store.ChatMessage) error { return nil },
	}, calls
}

func taskArgs(model, effort string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"tasks": []map[string]string{{
		"description": "lookup",
		"prompt":      "find it",
		"model":       model,
		"effort":      effort,
	}}})
	return b
}

// TestDispatchCarriesModelAndEffort covers the durable agent's path.
func TestDispatchCarriesModelAndEffort(t *testing.T) {
	svc, calls := modelEffortService(t)

	if _, err := svc.StartBatch(context.Background(), "alice", taskArgs(provider.MockModelLarge, "low")); err != nil {
		t.Fatalf("StartBatch: %v", err)
	}

	call := <-calls
	if call.Spec.Model != provider.MockModelLarge {
		t.Errorf("Spec.Model = %q, want the per-task model", call.Spec.Model)
	}
	if call.Spec.Effort != "low" {
		t.Errorf("Spec.Effort = %q, want the per-task effort", call.Spec.Effort)
	}
}

// TestNestedDispatchCarriesModelAndEffort covers a sub-lead choosing
// per-worker. A sub-lead splitting a task is exactly where cheap models
// pay off — its workers are usually narrow and mechanical — so losing
// the choice here would quietly force every nested worker onto the
// default.
func TestNestedDispatchCarriesModelAndEffort(t *testing.T) {
	svc, calls := modelEffortService(t)
	registerTestJob(svc, "lead1", "alice", 1, subagentJobRunning)

	done := make(chan error, 1)
	go func() {
		_, err := svc.RunNestedBatch(context.Background(), "alice", "lead1", taskArgs(provider.MockModelLarge, "medium"))
		done <- err
	}()

	call := <-calls
	if call.Spec.Model != provider.MockModelLarge {
		t.Errorf("nested Spec.Model = %q, want the per-task model", call.Spec.Model)
	}
	if call.Spec.Effort != "medium" {
		t.Errorf("nested Spec.Effort = %q, want the per-task effort", call.Spec.Effort)
	}
	if call.Spec.Depth != 2 {
		t.Errorf("nested Spec.Depth = %d, want 2", call.Spec.Depth)
	}
	if err := <-done; err != nil {
		t.Fatalf("RunNestedBatch: %v", err)
	}
}

// TestUnsetModelAndEffortResolveToTheFleetDefaults proves omission is
// resolved in core, once, and the spec the pod runs carries the result.
//
// The UI reads the job record, not the pod, so a blank would leave
// every chip saying nothing about the tier a task was burning. Both
// sides ask the provider (Defaults().Subagent), so the default is
// decided once.
// TestUnsetModelAndEffortResolveOnceAndReachEveryReader follows the
// same value through the registry and meta.json.
func TestUnsetModelAndEffortResolveToTheFleetDefaults(t *testing.T) {
	svc, calls := modelEffortService(t)

	if _, err := svc.StartBatch(context.Background(), "alice", taskArgs("", "")); err != nil {
		t.Fatalf("StartBatch: %v", err)
	}

	call := <-calls
	if want := (provider.MockProvider{}).Defaults().Subagent; call.Spec.Model != want || call.Spec.Effort != "" {
		t.Errorf("Spec.Model = %q Spec.Effort = %q, want the fleet defaults %q and %q resolved in core",
			call.Spec.Model, call.Spec.Effort, (provider.MockProvider{}).Defaults().Subagent, "")
	}
}

// TestUnsetEffortResolvesToTheModelsDefault: an effort left unset is
// the chosen model's default effort (Provider.Effort), resolved in
// core like the model.
func TestUnsetEffortResolvesToTheModelsDefault(t *testing.T) {
	svc, calls := modelEffortService(t)
	if _, err := svc.StartBatch(context.Background(), "alice", taskArgs(provider.MockModelLarge, "")); err != nil {
		t.Fatalf("StartBatch: %v", err)
	}
	if call := <-calls; call.Spec.Effort != "high" {
		t.Errorf("Spec.Effort = %q, want the model's default (high)", call.Spec.Effort)
	}
}

// TestDispatchRefusesAnEffortTheModelDoesNotOffer: the schema's effort
// enum is every level any model takes, so a dispatch can name a level
// its model lacks; validation through Provider.Effort refuses it with
// that model's own list before anything spawns, on both paths.
func TestDispatchRefusesAnEffortTheModelDoesNotOffer(t *testing.T) {
	svc, _ := modelEffortService(t)
	registerTestJob(svc, "lead1", "alice", 1, subagentJobRunning)
	cases := []struct{ model, effort, want string }{
		{provider.MockModelLarge, "max", "low, medium, high"},
		{provider.MockModelSmall, "low", "takes no effort"},
		{"", "low", "takes no effort"}, // the default model has none
	}
	for _, tc := range cases {
		_, err := svc.StartBatch(context.Background(), "alice", taskArgs(tc.model, tc.effort))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("StartBatch(%q, %q) = %v, want an error mentioning %q", tc.model, tc.effort, err, tc.want)
		}
		_, err = svc.RunNestedBatch(context.Background(), "alice", "lead1", taskArgs(tc.model, tc.effort))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("RunNestedBatch(%q, %q) = %v, want an error mentioning %q", tc.model, tc.effort, err, tc.want)
		}
	}
}
