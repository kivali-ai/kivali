import React from 'react';

export function Skeleton({ width = '100%', height = 14, round = false }) {
  return <span className="kv-skeleton" aria-hidden="true" style={{ width, height, borderRadius: round ? 9999 : undefined }} />;
}
