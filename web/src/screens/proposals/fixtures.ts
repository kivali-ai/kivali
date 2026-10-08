// Proposal fixtures. The three Go golden files are what the server really emits; hire and handbook are
// written against the generated types.
import offboardJson from '../../../../internal/web/apitypes/testdata/proposal_offboard.json';
import reorgJson from '../../../../internal/web/apitypes/testdata/proposal_reorg.json';
import roleUpdateJson from '../../../../internal/web/apitypes/testdata/proposal_role_update.json';
import type { Proposal } from '../../api/types.gen';

/** The generated type with every string union widened to string: what a JSON import can be checked against. */
type Wire<T> = T extends string
  ? string
  : T extends number
    ? number
    : T extends boolean
      ? boolean
      : T extends readonly (infer U)[]
        ? Wire<U>[]
        : T extends object
          ? { [K in keyof T]: Wire<T[K]> }
          : T;

export const goRoleUpdate = (roleUpdateJson satisfies Wire<Proposal>) as Proposal;
export const goReorg = (reorgJson satisfies Wire<Proposal>) as Proposal;
export const goOffboard = (offboardJson satisfies Wire<Proposal>) as Proposal;

export const hire: Proposal = {
  path: 'messages/2026-09-28/0002-chief-of-staff.md',
  kind: 'hire',
  title: 'Hire a newsletter editor',
  proposer: { slug: 'chief-of-staff', name: 'Chief of Staff' },
  proposed_at: '2026-09-28T09:00:00Z',
  reason_md: 'Support lead writes the newsletter by hand. An editor would **draft** each edition.',
  attachments: [],
  summary: {
    agent: {
      slug: 'newsletter-editor',
      name: 'Newsletter editor',
      role_title: 'Newsletter editor',
      icon: 'newspaper',
      reports_to: { slug: 'support-lead', name: 'Support lead' },
      model: 'claude-sonnet-4-6',
      model_label: 'Sonnet 4.6',
      effort: 'medium',
    },
    moves: [],
    facts: [
      { label: 'Reports to', value: 'Support lead' },
      { label: 'Model', value: 'Sonnet 4.6 · medium' },
    ],
  },
  docs: [
    {
      key: 'role',
      title: 'Proposed role',
      meta: 'role.md · opening and 2 sections',
      after: 'You write the monthly newsletter.\n\n## What you own\nThe newsletter.\nThe archive page.\n\n## How you work\nDraft from closed assignments.\n',
    },
    {
      key: 'memory',
      title: 'Initial memory',
      meta: 'optional · 1 note',
      after: '## Readers\nKeep it under 600 words.\n',
    },
  ],
};

export const handbook: Proposal = {
  path: 'messages/2026-09-28/0004-chief-of-staff.md',
  kind: 'handbook_update',
  title: 'Spend above $2,000 needs your approval',
  proposer: { slug: 'chief-of-staff', name: 'Chief of Staff' },
  proposed_at: '2026-09-27T09:00:00Z',
  reason_md: 'Two agents spent near $2,000 with only a manager sign-off.',
  attachments: [],
  summary: { moves: [], facts: [{ label: 'Applies to', value: 'All 6 agents' }] },
  docs: [
    {
      key: 'handbook',
      title: 'Handbook',
      meta: 'handbook.md · before and after',
      before: 'This is how every agent works.\n\n## Money\nSpend only on assigned work.\nManagers approve spend above $5,000.\n\n## Writing\nPlain words.\n',
      after: 'This is how every agent works.\n\n## Money\nSpend only on assigned work.\nSpend above $2,000 needs Maya approval.\n\n## Writing\nPlain words.\n',
    },
  ],
};
