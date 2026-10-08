The pacing dial for the queue: how long an agent-to-agent message waits before it is delivered.

```jsx
<AutoRelease value="30s" onChange={setStop} />
<AutoRelease value="Off" variant="select" onChange={setStop} />
```

- Six fixed stops: Now · 30s · 2m · 5m · 20m · Off. Off holds everything until the person releases it; 30s is the normal state.
- `variant="slider"` (default) is one click per stop, for the desktop sidebar foot and the Queue header. `variant="select"` is for phone.
- The label sits left of the track so it reads as one control. The current stop is named in ink under the track; the others are ink-faint.
- Every instance writes the same server setting; queue countdowns follow it and Off cancels them.
