A single-series bar chart for a quantity per day, such as spend.

```jsx
<DayBars values={daily} labels={[[0, '30 Aug'], [14, '13 Sept'], [29, 'Today']]} />
```

- One colour (`--chart-lake`), so it needs no legend. Say "who" with `RankedBars` beside it rather than stacking.
- Bars have 2 px paper gaps. There is one y-axis with a dashed hairline grid, and ink-muted mono labels.
- Three x labels: start, middle and Today. The exact value shows on hover.
- Pass a smaller `width` on phone (about 324) so labels keep their size.
