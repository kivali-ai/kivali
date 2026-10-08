A full-width message about the page or the whole org: a notice, a warning, an error or a confirmation.

```jsx
<Banner tone="warning" title="Weather service unreachable">Retrying in 5 minutes.</Banner>
```

- `tone`: `info` (`cobalt-soft`), `warning` (`signal-soft`, needs the person), `danger` (something failed), `success`. Each carries its own icon, so color is never the only cue.
- `title` is the one-line point; children add a sentence. Errors say what happened, then what happens next or what to do.
- `action` holds one `sm` button; `onDismiss` adds a close button.
- Environment and system warnings (production, clock drift) are `danger` or `warning` banners at the top of the page.
