A block of code, a command or a raw payload, with a copy button.

```jsx
<CodeBlock language="bash" code="kivali hire --role gardener" />
```

- `code` is the text; `title` or `language` labels the header ("Terminal", "role.md", "json").
- Set in `code` (IBM Plex Mono) on `paper-raised` with a hairline border; long lines scroll sideways rather than wrap.
- Use it for things people copy or inspect. Tool inputs and outputs in chat use the same look inside `ToolCall`.
