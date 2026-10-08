import { describe, expect, it } from 'vitest';
import { identityColors } from '../ds';
import { agentIdentity, identityColorFor } from './agentIdentity';

describe('identityColorFor', () => {
  it('is stable and one of the identity colours', () => {
    for (const slug of ['chief-of-staff', 'engineering-lead', 'bookkeeper', 'garden-advisor', 'test-runner']) {
      const c = identityColorFor(slug);
      expect(identityColors).toContain(c);
      expect(identityColorFor(slug)).toBe(c);
    }
  });

  it('spreads slugs over more than one colour', () => {
    const slugs = Array.from({ length: 40 }, (_, i) => 'agent-' + i);
    expect(new Set(slugs.map(identityColorFor)).size).toBeGreaterThan(4);
  });

  it('follows the slug, not the name', () => {
    const a = agentIdentity({ slug: 'engineering-lead', name: 'Engineering lead', icon: 'code' });
    const renamed = agentIdentity({ slug: 'engineering-lead', name: 'Head of engineering', icon: 'code' });
    expect(renamed.color).toBe(a.color);
  });
});

describe('agentIdentity', () => {
  it('draws the icon the agent record names', () => {
    expect(agentIdentity({ slug: 'engineering-lead', name: 'Engineering lead', icon: 'code' })).toEqual({
      name: 'Engineering lead',
      role: 'code',
      color: identityColorFor('engineering-lead'),
    });
  });

  it('leaves the icon out, for initials only, when the record has none the avatar draws', () => {
    for (const icon of [undefined, '', 'robot']) {
      expect(agentIdentity({ slug: 'tester', name: 'tester', icon })).toEqual({ name: 'tester', color: identityColorFor('tester') });
    }
  });

  it('never reads an icon off the role title or name', () => {
    expect(agentIdentity({ slug: 'bookkeeper', name: 'Bookkeeper' }).role).toBeUndefined();
  });
});
