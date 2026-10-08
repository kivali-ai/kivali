Buttons start or confirm an action; the label is a verb in sentence case ("Hire agent", "Release all").

```jsx
<Button variant="primary" icon={<Icon name="user-plus" />}>Hire agent</Button>
```

- `variant`: `primary` (an `ink` fill; one per view, the main action), `secondary` (default, a bordered `paper-raised` button), `ghost` (a soft `line`-colored fill with no border: the quietest button that still reads as a button, for toolbars and "skip"-type actions), `danger` (a `danger` fill, only for destructive actions such as offboarding or deleting files; pair it with a confirm `Dialog`).
- `size`: exactly two. `md` (36px) is the default everywhere; `sm` (28px) only inside dense rows and cards (inbox items, table rows). Never mix sizes within one group of buttons.
- Cancel in a dialog or form is `secondary`, not `ghost`, so both choices look like buttons.
- `loading`: swaps the icon for three bouncing dots and sets `aria-busy`; keep the label.
- `icon` puts an icon before the label; `iconOnly` hides the label (give an `aria-label`).
- The consumer provides the label and `onClick`; everything else of a native `<button>` passes through.
