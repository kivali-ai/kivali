A modal for a decision that needs full attention: confirming a destructive action, or a short form. Built on Radix Dialog (focus trap, Escape to close, labelled title).

```jsx
<Dialog trigger={<Button>Offboard</Button>} tone="danger" title="Offboard Garden advisor?" footer={<><DialogClose asChild><Button>Cancel</Button></DialogClose><Button variant="danger">Offboard</Button></>} />
```

- `title` asks the question ("Offboard Garden advisor?"); `description` says what will happen; `footer` holds the buttons, cancel (`secondary`) first and the action last.
- `tone="danger"` colors the title for destructive confirmations; the action button is then `danger`.
- Control it with `open` and `onOpenChange`, or give it a `trigger` element. Wrap cancel in `DialogClose`.
- Replace every browser `confirm()` with this.
