package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/supervisor/guestapi"
)

// agentPodLabel selects the agent pods the server provisions
// (internal/agentpod).
const agentPodLabel = "app=kivali-agentpod"

// agentPodFailures is how many failed runs of an agent pod's container
// (or init container) mean the release's pod spec or image is broken:
// one crash can be the runtime starting before core's socket answers,
// and it restarts; a broken spec fails the same way every time.
const agentPodFailures = 3

// agentImageWaiting are the waiting reasons of an agent pod's container
// that mean the release's spec or image is broken at once: an image the
// airgapped VM does not have, or a spec the kubelet cannot turn into a
// container.
var agentImageWaiting = map[string]bool{
	"ErrImagePull":               true,
	"ImagePullBackOff":           true,
	"ErrImageNeverPull":          true,
	"InvalidImageName":           true,
	"CreateContainerConfigError": true,
}

// agentPodList is the part of the agent pods waitAgents reads.
type agentPodList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			Containers []struct {
				Name  string `json:"name"`
				Image string `json:"image"`
			} `json:"containers"`
		} `json:"spec"`
		Status struct {
			ContainerStatuses     []containerStatus `json:"containerStatuses"`
			InitContainerStatuses []containerStatus `json:"initContainerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

// agentPodsReadiness is the server's GET /readyz/agents body
// (internal/web, agentPodsReadiness): how many agents have a pod, and
// how many of those pods' runtimes are connected to the server and
// taking commands on the spec it provisioned them with.
type agentPodsReadiness struct {
	AgentPods int `json:"agent_pods"`
	Connected int `json:"connected"`
}

// errNoAgentReadiness is a server that does not answer /readyz/agents:
// a release from before it.
var errNoAgentReadiness = errors.New("the server does not report its agents")

// agentPodsBroken returns why an agent pod whose agent container runs
// image (the release's server image: the agent runs the same binary)
// cannot start, or nil. A pod on another image is the previous
// release's, which the new server recreates. pending describes the
// containers of the pods on image that are still waiting.
func agentPodsBroken(pl agentPodList, image string) (pending []string, err error) {
	for _, p := range pl.Items {
		onImage := false
		for _, c := range p.Spec.Containers {
			if c.Name == "agent" && guestapi.NormalizeRef(c.Image) == guestapi.NormalizeRef(image) {
				onImage = true
			}
		}
		if !onImage {
			continue
		}
		check := func(kind string, cs []containerStatus) error {
			for _, c := range cs {
				if w := c.State.Waiting; w != nil && agentImageWaiting[w.Reason] {
					return fmt.Errorf("agent pod %s %s %s: %s: %s", p.Metadata.Name, kind, c.Name, w.Reason, w.Message)
				}
				if n, _, last := c.failedRuns(); n >= agentPodFailures {
					why := "exit"
					if last != nil {
						why = fmt.Sprintf("exit %d", last.ExitCode)
						if m := strings.TrimSpace(last.Message); m != "" {
							why += ": " + m
						} else if last.Reason != "" {
							why += " (" + last.Reason + ")"
						}
					}
					return fmt.Errorf("agent pod %s %s %s failed %d times: %s", p.Metadata.Name, kind, c.Name, n, why)
				}
				if w := c.State.Waiting; w != nil {
					pending = append(pending, fmt.Sprintf("%s/%s %s", p.Metadata.Name, c.Name, w.Reason))
				}
			}
			return nil
		}
		if err := check("init container", p.Status.InitContainerStatuses); err != nil {
			return nil, err
		}
		if err := check("container", p.Status.ContainerStatuses); err != nil {
			return nil, err
		}
	}
	return pending, nil
}

// agentReadiness asks the server, through the forward, how many of its
// agent pods are connected. errNoAgentReadiness when it does not say.
func (s *Supervisor) agentReadiness(ctx context.Context) (agentPodsReadiness, error) {
	var r agentPodsReadiness
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(c, http.MethodGet, s.baseURL()+"/readyz/agents", nil)
	if err != nil {
		return r, err
	}
	resp, err := s.o.HTTP.Do(req)
	if err != nil {
		return r, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return r, err
	}
	switch resp.StatusCode {
	case http.StatusNotFound:
		return r, errNoAgentReadiness
	case http.StatusOK, http.StatusServiceUnavailable:
		if err := json.Unmarshal(body, &r); err != nil {
			return r, fmt.Errorf("/readyz/agents: %s", resp.Status)
		}
		return r, nil
	}
	return r, fmt.Errorf("/readyz/agents: %s", resp.Status)
}

// waitAgents waits, after the server answers /readyz, for every agent
// pod the server runs to be up on the release's image with its runtime
// connected to the server and taking commands (/readyz/agents answers
// 200). A team with no agent pods passes at once. It fails at once when
// an agent pod on image cannot start for a reason the release causes
// (agentPodsBroken), and when deployTimeout passes. A server that does
// not answer /readyz/agents is not waited for.
func (s *Supervisor) waitAgents(ctx context.Context, image string, logf Logf) error {
	g, err := s.running()
	if err != nil {
		return err
	}
	start := s.o.Clock.Now()
	what := "the agents to connect to the server"
	logf("waiting for %s", what)
	var connected agentPodsReadiness
	err = poll(ctx, s.o.Clock, deployTimeout, pollInterval, what, func(ctx context.Context) error {
		var pending []string
		if out, err := kubectl(ctx, g, "-n", Namespace, "get", "pods", "-l", agentPodLabel, "-o", "json"); err == nil {
			var pl agentPodList
			if json.Unmarshal(out, &pl) == nil {
				var berr error
				if pending, berr = agentPodsBroken(pl, image); berr != nil {
					return permanent(berr)
				}
			}
		}
		r, err := s.agentReadiness(ctx)
		if err != nil {
			if errors.Is(err, errNoAgentReadiness) {
				return permanent(err)
			}
			return err
		}
		connected = r
		if r.Connected >= r.AgentPods {
			return nil
		}
		msg := fmt.Sprintf("%d of %d agents connected", r.Connected, r.AgentPods)
		if len(pending) > 0 {
			msg += "; waiting: " + strings.Join(pending, ", ")
		}
		return errors.New(msg)
	})
	if errors.Is(err, errNoAgentReadiness) {
		logf("%s; not waiting for them", errNoAgentReadiness)
		return nil
	}
	if err != nil {
		return err
	}
	logf("%d of %d agents connected after %s", connected.Connected, connected.AgentPods, s.o.Clock.Now().Sub(start).Round(time.Second))
	return nil
}
