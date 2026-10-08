import { describe, expect, it } from 'vitest';
import manifest from '../../../design-system/_ds_manifest.json';
import * as ds from './index';

// Runtime helpers the reference .d.ts files declare beside the components.
const HELPERS = [
  'ROLE_ICONS',
  'roleIconNames',
  'identityColors',
  'initialsFor',
  'colorFor',
  'ICONS',
  'iconNames',
  'AssignmentGlyph',
  'usePopover',
  'splitSections',
  'AUTO_RELEASE_STOPS',
  'DiffLine',
  'MenuItem',
  'DialogClose',
];

// Components the vendored design system lists that this app does not port. OrgSwitcher: the server has one org
// per deployment, so the frame shows the org's mark and name without a switcher.
const NOT_PORTED = ['OrgSwitcher'];

describe('src/ds barrel', () => {
  it('exports every component the design system manifest lists', () => {
    const names = (manifest as { components: { name: string }[] }).components.map((c) => c.name);
    expect(names.length).toBeGreaterThan(40);
    const missing = names.filter((n) => !NOT_PORTED.includes(n) && !(n in ds));
    expect(missing).toEqual([]);
  });

  it('exports the helpers the reference .d.ts files declare', () => {
    const missing = HELPERS.filter((n) => !(n in ds));
    expect(missing).toEqual([]);
  });

  it('exports the Kivali additions', () => {
    expect('Text' in ds).toBe(true);
    expect('TooltipProvider' in ds).toBe(true);
  });
});
