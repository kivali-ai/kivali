Where the person writes to an agent.

```jsx
<Composer placeholder="Message Garden advisor" onSend={(v) => {}} onAttach={() => {}} />
```

- The text area grows with the message up to about ten lines, then scrolls. Enter sends; Shift+Enter adds a line.
- `onSend(text)` is called on send; `busy` swaps the send icon for three dots and blocks sending while the agent works. `onAttach` adds a paperclip; `footer` holds quiet details like the model and attachments as outline `Badge`s.
- The placeholder names who you're talking to ("Message Garden advisor").
- `allowEmpty`: lets the message go out with no text, when the consumer has something else to send (staged attachments).
