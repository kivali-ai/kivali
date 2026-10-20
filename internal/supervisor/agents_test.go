package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// upgradeTo016 upgrades h's org to 0.16.0 and returns the error.
func upgradeTo016(t *testing.T, h *harness) error {
	t.Helper()
	feed := writeRelease(t, filepath.Join(t.TempDir(), "rel"), "0.16.0", "v0.16.0")
	return h.sup.Upgrade(context.Background(), UpgradeOptions{Feed: feed}, h.logf())
}

// A team update must not commit before the agents run on the new
// release: /readyz says only that the server can write its data, and
// agent pods with the new spec are created after it. Committing first
// would leave a team whose agents never came back with no way to the
// release it had. The update asks the server until every agent pod's
// runtime is connected, then commits.
func TestUpgradeWaitsForTheAgentsBeforeCommitting(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	h.vm.mu.Lock()
	h.vm.readyzAgentsLooks = 0
	h.vm.readyzAgents = func(n int) (int, string) {
		if n < 3 {
			return http.StatusServiceUnavailable, fmt.Sprintf(`{"agent_pods":2,"connected":%d}`, n-1)
		}
		return http.StatusOK, `{"agent_pods":2,"connected":2}`
	}
	h.vm.mu.Unlock()
	if err := upgradeTo016(t, h); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if h.vm.readyzAgentsLooks != 3 {
		t.Fatalf("/readyz/agents asked %d times, want 3: the commit must wait for both agents", h.vm.readyzAgentsLooks)
	}
	if st := h.sup.State(); st.Kivali != "0.16.0" || st.Upgrade != nil {
		t.Fatalf("state %+v", st)
	}
}

// An update whose agent pods cannot start on the new spec or image
// rolls back at once with the pod's reason: waiting out the timeout
// would keep the team down for ten minutes for nothing, and an error
// without the reason leaves nobody able to say what broke.
func TestUpgradeRollsBackWhenAnAgentPodCannotStart(t *testing.T) {
	for _, c := range []struct {
		name, status, want string
	}{
		{"image", `{"name":"agent","restartCount":0,"state":{"waiting":{"reason":"ImagePullBackOff","message":"image not present"}}}`,
			"agent pod agentpod-chief-of-staff container agent: ImagePullBackOff: image not present"},
		{"crash loop", `{"name":"agent","restartCount":2,"state":{"waiting":{"reason":"CrashLoopBackOff"}},"lastState":{"terminated":{"exitCode":1,"reason":"Error"}}}`,
			"agent pod agentpod-chief-of-staff container agent failed 3 times: exit 1 (Error)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.mustUp(UpOptions{Install: owner})
			before := h.disk()[ManifestRel]
			h.vm.agentContainer = c.status
			err := upgradeTo016(t, h)
			if err == nil || !strings.Contains(err.Error(), "still on 0.15.0") || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("upgrade: %v", err)
			}
			if h.sup.State().Kivali != "0.15.0" || h.disk()[ManifestRel] != before {
				t.Fatalf("not rolled back: %+v", h.sup.State())
			}
		})
	}
}

// Agents that never connect, for whatever reason, roll the update back
// once the rollout timeout passes, saying how many connected: the owner
// asked that an update commit only on agents that are running,
// connected and ready for commands.
func TestUpgradeRollsBackWhenTheAgentsNeverConnect(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	h.vm.mu.Lock()
	h.vm.readyzAgents = func(int) (int, string) {
		return http.StatusServiceUnavailable, `{"agent_pods":1,"connected":0}`
	}
	h.vm.mu.Unlock()
	err := upgradeTo016(t, h)
	if err == nil || !strings.Contains(err.Error(), "still on 0.15.0") || !strings.Contains(err.Error(), "timed out after 10m0s waiting for the agents to connect to the server (last: 0 of 1 agents connected)") {
		t.Fatalf("upgrade: %v", err)
	}
	if h.sup.State().Kivali != "0.15.0" {
		t.Fatalf("not rolled back: %+v", h.sup.State())
	}
}

// A release whose server predates /readyz/agents (a downgrade, or a
// forced update to an older release) still updates: its 404 means the
// server cannot say, not that the agents are down, and treating it as a
// failure would make every such update roll back after ten minutes.
func TestUpgradeToAServerThatDoesNotReportItsAgents(t *testing.T) {
	h := newHarness(t)
	h.mustUp(UpOptions{Install: owner})
	h.vm.mu.Lock()
	h.vm.readyzAgents = func(int) (int, string) { return http.StatusNotFound, "" }
	h.vm.mu.Unlock()
	if err := upgradeTo016(t, h); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if h.sup.State().Kivali != "0.16.0" {
		t.Fatalf("state %+v", h.sup.State())
	}
}

// agentPodsBroken judges only pods on the release's image, and only
// failures the release causes: a previous release's pod is about to be
// recreated, and one crash can be a runtime that started before the
// server answered. Judging either would roll back a good update; missing
// a failing init container would wait out the timeout on a broken one.
func TestAgentPodsBroken(t *testing.T) {
	pods := func(image, field, status string) agentPodList {
		var pl agentPodList
		js := fmt.Sprintf(`{"items":[{"metadata":{"name":"agent-a"},"spec":{"containers":[{"name":"agent","image":%q}]},"status":{%q:[%s]}}]}`, image, field, status)
		if err := json.Unmarshal([]byte(js), &pl); err != nil {
			t.Fatal(err)
		}
		return pl
	}
	const image = "docker.io/library/kivali:v0.16.0"
	pull := `{"name":"agent","state":{"waiting":{"reason":"ImagePullBackOff"}}}`
	if _, err := agentPodsBroken(pods("kivali:v0.15.0", "containerStatuses", pull), image); err != nil {
		t.Errorf("previous release's pod judged: %v", err)
	}
	if _, err := agentPodsBroken(pods("kivali:v0.16.0", "containerStatuses", pull), image); err == nil {
		t.Error("image pull failure on the release's image not caught")
	}
	once := `{"name":"agent","restartCount":0,"state":{"waiting":{"reason":"CrashLoopBackOff"}},"lastState":{"terminated":{"exitCode":1}}}`
	pending, err := agentPodsBroken(pods(image, "containerStatuses", once), image)
	if err != nil || len(pending) != 1 || pending[0] != "agent-a/agent CrashLoopBackOff" {
		t.Errorf("one crash: pending %v, err %v", pending, err)
	}
	initFails := `{"name":"prepare-scratch","restartCount":2,"state":{"waiting":{"reason":"CrashLoopBackOff"}},"lastState":{"terminated":{"exitCode":2,"message":"no space"}}}`
	if _, err := agentPodsBroken(pods(image, "initContainerStatuses", initFails), image); err == nil || !strings.Contains(err.Error(), "init container prepare-scratch failed 3 times: exit 2: no space") {
		t.Errorf("failing init container: %v", err)
	}
}
