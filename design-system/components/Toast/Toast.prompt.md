A brief confirmation or error after something the person did, stacked in the bottom-right corner (or bottom-center on small screens).

```jsx
<Toast tone="success" title="Brief saved">Garden advisor picks it up on its next run.</Toast>
```

- `tone`: `success` (done), `info` (neutral news, often with Undo), `danger` (the action failed). Each has its own icon.
- `title` is the one-line result ("Skill installed"); children add one sentence. `action` holds one `sm` button such as Undo; `onDismiss` adds a close button.
- Success and info toasts leave on their own after about five seconds (longer when they hold an action); danger toasts stay until dismissed. Things that need a decision belong in the inbox, not a toast.
- Stack toasts in one fixed region at `z-toast`; wire them with Radix Toast or any toast queue that keeps these rules.
