import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { SubagentGroup } from './SubagentGroup';

describe('SubagentGroup', () => {
  it('counts the tasks and sums up where they stand', () => {
    render(
      <SubagentGroup
        tasks={[
          { title: 'A', state: 'running' },
          { title: 'B', state: 'done' },
          { title: 'C', state: 'done' },
        ]}
      />,
    );
    expect(screen.getByText('3 subagents')).toBeInTheDocument();
    expect(screen.getByText('1 running · 2 done')).toBeInTheDocument();
    expect(screen.getByText('A')).toBeInTheDocument();
  });

  it('says failed and uses the worst state', () => {
    const { container } = render(
      <SubagentGroup
        tasks={[
          { title: 'A', state: 'done' },
          { title: 'B', state: 'errored', error: 'no' },
        ]}
      />,
    );
    expect(screen.getByText('1 done · 1 failed')).toBeInTheDocument();
    expect(container.querySelector('.kv-subgroup')).toHaveClass('kv-sub--errored');
  });

  it('uses the singular for one task and accepts children', () => {
    render(
      <SubagentGroup tasks={[{ title: 'Only', state: 'done' }]}>
        <p>Custom body</p>
      </SubagentGroup>,
    );
    expect(screen.getByText('1 subagent')).toBeInTheDocument();
    expect(screen.getByText('Custom body')).toBeInTheDocument();
  });
});
