import { render } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { describe, expect, it } from 'vitest';
import { identityColorFor } from '../lib/agentIdentity';
import { buildTree } from '../state/org';
import { goSnapshot } from '../state/fixtures';
import { TeamTree } from './TeamTree';

describe('TeamTree', () => {
  it('gives each agent the identity colour Home gives it (from the slug)', () => {
    const { container } = render(
      <MemoryRouter>
        <TeamTree tree={buildTree(goSnapshot.agents)} person="Maya Chen" activeSlug={null} />
      </MemoryRouter>,
    );
    const tiles = [...container.querySelectorAll<HTMLElement>('.kv-avatar--agent')];
    expect(tiles).toHaveLength(goSnapshot.agents.length);
    tiles.forEach((tile, i) => {
      const agent = goSnapshot.agents[i];
      if (!agent) throw new Error('no agent ' + i);
      expect(tile).toHaveAccessibleName(agent.name || agent.slug);
      expect(tile.style.background).toBe('var(--identity-' + identityColorFor(agent.slug) + ')');
    });
  });
});
