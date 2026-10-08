Styling for rendered markdown: agent roles, memory, the handbook, assignment descriptions, docs.

```jsx
<Prose html={markdownHtml} />
```

- Wrap rendered HTML with `html`, or pass elements as children. Text sets in `body` at a comfortable measure (about 68 characters).
- Headings use the display face, links are `link` (cobalt) and underlined, inline code sits on a `line` tint, blockquotes get a thin `ink-faint` rule, and fenced code gets the `CodeBlock` look.
- Sanitize any agent- or user-written markdown before passing it as `html`.
- Prose h4–h6 render as body-size titles in the title weight (from `components/text.css`).
