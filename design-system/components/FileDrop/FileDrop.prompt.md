Where people add files: project files at setup, the files page, skills.

```jsx
<FileDrop hint="PDF, CSV or images, up to 20MB" onFiles={(f) => {}} />
```

- `label` names what goes here; `hint` states types and limits. `accept` and `multiple` pass to the file input; `onFiles(files)` receives the list from a drop or the picker.
- The whole area is one target: click, Enter or drop. It tints `cobalt-soft` while a file hovers over it.
- Show what was added as `ListRow`s beneath it, each with a remove action.
