Tells the person an agent's chat is nearly full, next to the place they can act on it.

```jsx
<ContextGauge value={64} onNewChat={rotate} />
<NavItem label="Chief of Staff" count={<><AgentState state="running" compact /><ContextCount value={86} /></>} />
```

- Under the threshold (default 80) it is a quiet gauge: teal under 50%, cobalt up to 80%, the percentage in words.
- Past the threshold it becomes a solid `signal` badge "Context 82% full" and New chat turns primary. No banner over the transcript.
- `ContextCount` goes in the org tree and on Team rows as a soft signal percentage; it renders nothing below the threshold.
- Temporary: remove when chat rotation is automatic.
