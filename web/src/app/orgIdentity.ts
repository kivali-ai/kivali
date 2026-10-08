import type { Me } from '../api/types.gen';

const UNNAMED = 'Your org';

/** Until the owner names the org the mark shows this; the name comes from Org > Organization. */
export function orgName(me: Me | null): string {
  return me?.org.name || UNNAMED;
}

/** The uploaded mark, a square PNG the server already serves as the favicon set. */
export const ORG_LOGO_SRC = '/branding/icon-180.png';
