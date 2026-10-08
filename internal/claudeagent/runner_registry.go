package claudeagent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/kivali-ai/kivali/internal/provider"
)

// runnerRegistry keeps one warm runner per agent slug, with no upper
// bound on the number of warm runners. Every active agent has its own
// long-lived `claude -p` subprocess; an agent that hasn't received a
// message since boot has none. There is intentionally no LRU
// eviction, no idle TTL, no concurrency cap:
//
//   - We don't yet have prod numbers on per-CLI RSS over a real
//     conversation, so any cap chosen now would be a guess. A cap +
//     LRU policy turns "lots of agents" into thrashing (evicted
//     runners get respawned on their next turn, paying full V8
//     warmup cost), which is exactly the problem persistent mode
//     was supposed to solve.
//   - If we do hit memory pressure under real load, we'll have
//     numbers to design against — bounded set, LRU, idle TTL, etc.
//     can come back as a real fix on a real signal, not a
//     pre-emptive guess.
//
// What the registry does today:
//   - Acquire(slug) → live runner, spawning if missing or if the
//     existing one's subprocess has died.
//   - Crash recovery: when a runner's subprocess exits unexpectedly,
//     its supervisor marks it dead; the next Acquire detects the
//     corpse, evicts it, and spawns fresh (using --resume to recover
//     conversation context from the on-disk session log).
//   - Close: drains every live runner in parallel during process
//     shutdown.
//
// One instance per Kivali process, owned by the claudeagent client.
type runnerRegistry struct {
	opts       Options
	kivaliBin  string
	sessionDir string

	// signIn keys the CLI's current sign-in; a runner born under
	// another key is respawned (see Acquire).
	signIn *signInWatch

	mu      sync.Mutex
	runners map[string]*runner

	closeOnce sync.Once
	closed    chan struct{}
}

// newRunnerRegistry constructs the registry. shutdownCtx, when
// non-nil, drains all warm runners when canceled (process SIGTERM).
func newRunnerRegistry(opts Options, kivaliBin, sessionDir string, shutdownCtx context.Context) (*runnerRegistry, error) {
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return nil, fmt.Errorf("claudeagent: registry session dir: %w", err)
	}
	r := &runnerRegistry{
		opts:       opts,
		kivaliBin:  kivaliBin,
		sessionDir: sessionDir,
		signIn:     newSignInWatch(opts.ClaudeBinary, opts.HomeDir),
		runners:    make(map[string]*runner),
		closed:     make(chan struct{}),
	}
	if shutdownCtx != nil {
		go func() {
			// Exit on EITHER the shutdown ctx or an explicit
			// registry Close — otherwise this goroutine sits forever
			// waiting on a ctx that may never cancel (e.g., in tests
			// that close the registry directly).
			select {
			case <-shutdownCtx.Done():
				_ = r.Close()
			case <-r.closed:
			}
		}()
	}
	return r, nil
}

// Acquire returns the live runner for slug, spawning one if the
// registry has none or if the existing one's subprocess has died.
// resumeID + req are used only when spawning fresh; an existing live
// runner ignores them (its in-memory CLI session is the source of
// truth past spawn time).
//
// A runner born under another sign-in (signInWatch) is respawned the
// same way as one whose config drifted: the CLI keeps the credentials
// and settings env it started with, so a new sign-in reaches a warm
// agent at the start of its next turn, never in the middle of one.
func (r *runnerRegistry) Acquire(ctx context.Context, slug, resumeID string, req provider.CompleteRequest) (*runner, error) {
	if slug == "" {
		return nil, errors.New("claudeagent: registry requires non-empty agent slug")
	}
	// Outside the registry lock: a changed sign-in costs a CLI run.
	signIn := r.signIn.Key(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()

	// Re-check the closed flag INSIDE the lock. The lock-and-check
	// order matters: Close also takes the registry mutex, so once
	// we hold it, the closed channel's state is stable for the
	// rest of this critical section. Without this, a concurrent
	// Close could drain the registry between the select-default
	// and the lock — Acquire would then spawn a fresh runner that
	// Close immediately tears down (caller sees a confusing
	// "runner is dead" on RunTurn).
	select {
	case <-r.closed:
		return nil, errors.New("claudeagent: registry is closed")
	default:
	}

	if existing, ok := r.runners[slug]; ok {
		if !existing.isDead() {
			if existing.configMatches(req) && existing.spawnSignIn == signIn {
				return existing, nil
			}
			// Config drift: the CEO changed this agent's Model or
			// Effort, or its handbook / role / agent_memory (all
			// carried in the system prompt) was edited, or the CLI
			// was signed in another way. The CLI pins model, effort,
			// system prompt and sign-in per subprocess. Evict
			// the live runner
			// so the spawn below respawns with the new flags. The
			// conversation is NOT lost — resumeID carries the stored
			// session id, so --resume reconstructs full context from the
			// CLI's on-disk log, the same way crash recovery does.
			// Synchronous Close here can block on graceful SIGTERM, but
			// it's bounded and only fires on an explicit settings/content
			// change (rare), not the hot path.
			log.Printf("claudeagent: %s runner config drift (model %q→%q, effort %q→%q, system-prompt changed=%v, sign-in changed=%v); respawning with --resume",
				slug, existing.spawnModel, req.Model, existing.spawnEffort, normalizeEffort(req.Effort),
				existing.spawnSystemHash != systemPromptHash(req), existing.spawnSignIn != signIn)
			delete(r.runners, slug)
			_ = existing.Close()
		} else {
			// Corpse — evict synchronously while holding the registry
			// mutex so a concurrent Acquire for the same slug can't race
			// past us and spawn a duplicate. Close() on a dead runner
			// returns essentially instantly (`<-r.dead` is already
			// closed), so the lock-hold cost is sub-millisecond.
			delete(r.runners, slug)
			_ = existing.Close()
		}
	}

	// newRunner is slow (subprocess spawn + V8 warmup ~hundreds of
	// ms) and we hold the lock across it. That serializes spawns of
	// different slugs but ensures one-runner-per-slug invariant
	// without racing. Until we measure spawn frequency in prod and
	// see this as a real bottleneck, the simpler design wins.
	fresh, err := newRunner(r.opts, slug, r.kivaliBin, r.sessionDir, resumeID, req)
	if err != nil {
		return nil, err
	}
	fresh.spawnSignIn = signIn
	r.runners[slug] = fresh
	return fresh, nil
}

// Evict tears down the warm runner for slug (if any) so the next
// Acquire spawns a fresh one. This is the rotation seam.
//
// On a chat rotation, core archives the chat and clears the stored
// CLI session id, expecting the next turn to start a clean session.
// But Acquire deliberately returns the existing live runner WITHOUT
// re-reading the session id — its in-memory CLI session is the source
// of truth past spawn time (see Acquire's doc). So a warm runner keeps
// its `claude -p` subprocess alive with the entire pre-rotation
// conversation in memory and would --resume it forever; clearing the
// on-disk id alone is inert. Evicting the runner is what actually
// forces the reset: the next Acquire finds no runner, reads the
// now-empty session id, and spawns without --resume — a genuinely
// fresh conversation.
//
// The map delete is synchronous under the registry lock so a
// concurrent Acquire for the same slug can't be handed back the runner
// we're discarding. The Close (which blocks on the subprocess flushing
// stdin and exiting, up to closeGraceTimeout) runs in the background so
// callers on the event-dispatch path aren't held for the grace window.
// No-op when no warm runner exists for slug.
func (r *runnerRegistry) Evict(slug string) {
	r.mu.Lock()
	existing, ok := r.runners[slug]
	if ok {
		delete(r.runners, slug)
	}
	r.mu.Unlock()
	if ok {
		go func() { _ = existing.Close() }()
	}
}

// Close drains the registry: closes every live runner in parallel,
// so total wait is bounded by one closeGraceTimeout. Idempotent.
func (r *runnerRegistry) Close() error {
	var firstErr error
	r.closeOnce.Do(func() {
		close(r.closed)
		r.mu.Lock()
		runners := make([]*runner, 0, len(r.runners))
		for _, rr := range r.runners {
			runners = append(runners, rr)
		}
		r.runners = nil
		r.mu.Unlock()
		var wg sync.WaitGroup
		errCh := make(chan error, len(runners))
		for _, rr := range runners {
			wg.Add(1)
			go func(target *runner) {
				defer wg.Done()
				if err := target.Close(); err != nil {
					errCh <- err
				}
			}(rr)
		}
		wg.Wait()
		close(errCh)
		for err := range errCh {
			if firstErr == nil {
				firstErr = err
				log.Printf("claudeagent: registry close: %v", err)
			}
		}
	})
	return firstErr
}

// registrySessionDirDefault returns the registry's working directory
// under the claudeagent session dir. Per-runner artifacts (stderr
// capture, MCP config) live here keyed by slug.
func registrySessionDirDefault(opts Options) string {
	base := opts.SessionDir
	if base == "" {
		base = filepath.Join(os.TempDir(), "kivali-claudeagent")
	}
	return filepath.Join(base, "registry")
}
