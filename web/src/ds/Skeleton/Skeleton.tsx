export interface SkeletonProps {
  width?: number | string;
  height?: number;
  round?: boolean;
}

/**
 * A placeholder shape while content loads, drawn roughly where the real content will be.
 *
 * - `width`, `height` (default 14) and `round` for avatars and pills. Match the layout closely so nothing jumps when the content arrives.
 * - Use it only past about 300ms of loading; faster loads should just appear. The shimmer stops for reduced motion.
 */
export function Skeleton({ width = '100%', height = 14, round = false }: SkeletonProps) {
  return <span className="kv-skeleton" aria-hidden="true" style={{ width, height, borderRadius: round ? 9999 : undefined }} />;
}
