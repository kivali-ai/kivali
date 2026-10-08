import { describe, expect, it } from 'vitest';
import roleIcons from '../../../../internal/web/apitypes/testdata/role_icons.json';
import { ROLE_ICONS } from './roleIcons';

describe('ROLE_ICONS', () => {
  it('draws exactly the icons the hiring agent picks from (internal/roleicon)', () => {
    expect(Object.keys(ROLE_ICONS).sort()).toEqual(roleIcons.map((i) => i.name).sort());
  });
});
