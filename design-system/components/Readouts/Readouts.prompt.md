A strip of counts at the top of a view: "5 agents working · 2 blocked · 9 closed this week · $14.20 today".

```jsx
<Readouts items={[{ n: '5', label: 'agents working', href: '/work?state=moving' }, { n: '2', label: 'blocked', href: '/work?state=blocked' }]} />
```

- The number is ink at 13 px and the label ink-muted mono at 11 px, separated by a middle dot.
- No boxes and no icons: it is a line of text. Each item links to its filtered view.
- Items wrap whole, never mid-phrase.
