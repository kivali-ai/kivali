A placeholder shape while content loads, drawn roughly where the real content will be.

```jsx
<Skeleton width={180} height={14} />
```

- `width`, `height` (default 14) and `round` for avatars and pills. Match the layout closely so nothing jumps when the content arrives.
- Use it only past about 300ms of loading; faster loads should just appear. The shimmer stops for reduced motion.
