package browserpod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// worldName names the isolated world the pod's scripts run in, apart
// from the page's own scripts.
const worldName = "kivali"

type remoteObject struct {
	Type                string          `json:"type"`
	Subtype             string          `json:"subtype"`
	Value               json.RawMessage `json:"value"`
	UnserializableValue string          `json:"unserializableValue"`
	Description         string          `json:"description"`
	ObjectID            string          `json:"objectId"`
}

type evalResponse struct {
	Result           remoteObject `json:"result"`
	ExceptionDetails *struct {
		Text      string        `json:"text"`
		Exception *remoteObject `json:"exception"`
	} `json:"exceptionDetails"`
}

func (r evalResponse) err() error {
	if r.ExceptionDetails == nil {
		return nil
	}
	msg := r.ExceptionDetails.Text
	if e := r.ExceptionDetails.Exception; e != nil && e.Description != "" {
		msg = e.Description
	}
	// The exception's text may quote the page.
	return failPage(CodeBrowser, "The script threw:", cut(msg, 500))
}

// world returns the execution context of the tab's isolated world,
// creating it for the current document when there is none.
func (b *browser) world(ctx context.Context, t *tab) (int, error) {
	t.mu.Lock()
	id := t.world
	t.mu.Unlock()
	if id != 0 {
		return id, nil
	}
	raw, err := b.conn.Call(ctx, t.session, "Page.createIsolatedWorld", map[string]any{
		"frameId": t.targetID, "worldName": worldName,
	})
	if err != nil {
		return 0, err
	}
	var res struct {
		ID int `json:"executionContextId"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return 0, err
	}
	t.update(func() { t.world = res.ID })
	return res.ID, nil
}

// isContextGone reports an error from a world that went away with
// its document.
func isContextGone(err error) bool {
	var ce *cdpError
	return errors.As(err, &ce) && (strings.Contains(ce.Message, "context") || strings.Contains(ce.Message, "Cannot find"))
}

func (b *browser) forgetWorld(t *tab, id int) {
	t.update(func() {
		if t.world == id {
			t.world = 0
		}
	})
}

// evaluate runs an expression in the tab's isolated world, awaiting a
// promise it returns.
func (b *browser) evaluate(ctx context.Context, t *tab, expr string, byValue bool) (remoteObject, error) {
	for attempt := 0; ; attempt++ {
		id, err := b.world(ctx, t)
		if err != nil {
			return remoteObject{}, err
		}
		raw, err := b.conn.Call(ctx, t.session, "Runtime.evaluate", map[string]any{
			"expression":    expr,
			"contextId":     id,
			"returnByValue": byValue,
			"awaitPromise":  true,
		})
		if err != nil {
			if attempt == 0 && isContextGone(err) {
				b.forgetWorld(t, id)
				continue
			}
			return remoteObject{}, err
		}
		var res evalResponse
		if err := json.Unmarshal(raw, &res); err != nil {
			return remoteObject{}, err
		}
		return res.Result, res.err()
	}
}

// evalValue evaluates expr and decodes its value into out.
func (b *browser) evalValue(ctx context.Context, t *tab, expr string, out any) error {
	obj, err := b.evaluate(ctx, t, expr, true)
	if err != nil {
		return err
	}
	if len(obj.Value) == 0 {
		return fmt.Errorf("browserpod: the expression returned %s", obj.Type)
	}
	return json.Unmarshal(obj.Value, out)
}

// callOn calls fn with this bound to the remote object, passing args
// by value, and decodes its result into out when out is not nil.
func (b *browser) callOn(ctx context.Context, t *tab, objectID, fn string, out any, args ...any) error {
	callArgs := make([]map[string]any, len(args))
	for i, a := range args {
		callArgs[i] = map[string]any{"value": a}
	}
	raw, err := b.conn.Call(ctx, t.session, "Runtime.callFunctionOn", map[string]any{
		"functionDeclaration": fn,
		"objectId":            objectID,
		"arguments":           callArgs,
		"returnByValue":       true,
		"awaitPromise":        true,
	})
	if err != nil {
		if isContextGone(err) {
			return failf(CodeNotFound, "The page changed; take a new snapshot.")
		}
		return err
	}
	var res evalResponse
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	if err := res.err(); err != nil {
		return err
	}
	if out == nil || len(res.Result.Value) == 0 {
		return nil
	}
	return json.Unmarshal(res.Result.Value, out)
}

// parseRef reads "e12" (or "[e12]", or "12") as 12.
func parseRef(ref string) (int, bool) {
	ref = strings.TrimSpace(ref)
	ref = strings.TrimPrefix(strings.TrimSuffix(ref, "]"), "[")
	ref = strings.TrimPrefix(ref, "e")
	n, err := strconv.Atoi(ref)
	return n, err == nil && n > 0
}

// resolveRef finds the element a ref from the tab's last snapshot
// names, returning its remote object id.
func (b *browser) resolveRef(ctx context.Context, t *tab, ref string) (string, refInfo, error) {
	n, ok := parseRef(ref)
	if !ok {
		return "", refInfo{}, failf(CodeBadArgs, "%q is not a ref; refs look like e12 and come from web_snapshot.", ref)
	}
	t.mu.Lock()
	has, refsNav, navs, refs := t.hasRefs, t.refsNav, t.navs, t.refs
	t.mu.Unlock()
	switch {
	case !has:
		return "", refInfo{}, failf(CodeNotFound, "This tab has no snapshot yet; take one with web_snapshot to get refs.")
	case refsNav != navs:
		return "", refInfo{}, failf(CodeNotFound, "Ref %s is from before the page changed; take a new snapshot.", ref)
	case n >= len(refs):
		return "", refInfo{}, failf(CodeNotFound, "There is no ref e%d in the last snapshot; take a new snapshot.", n)
	}
	obj, err := b.evaluate(ctx, t, fmt.Sprintf(`(() => { const r = window.__kivaliRefs; const e = r && r[%d]; return e && e.isConnected ? e : null; })()`, n), false)
	if err != nil {
		return "", refInfo{}, err
	}
	if obj.ObjectID == "" {
		return "", refInfo{}, failf(CodeNotFound, "Ref e%d is no longer on the page; take a new snapshot.", n)
	}
	return obj.ObjectID, refs[n], nil
}
