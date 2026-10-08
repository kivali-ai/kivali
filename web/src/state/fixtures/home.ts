// Home fixtures. `goHome` and `goHistory` are the Go golden files, so the screen is tested against what the
// server really emits; the rest are written against the generated types.
import goHistoryJson from '../../../../internal/web/apitypes/testdata/home_history.json';
import goHomeJson from '../../../../internal/web/apitypes/testdata/home.json';
import goNeedActionJson from '../../../../internal/web/apitypes/testdata/need_action_response.json';
import type { Goal, HistoryResponse, Home, NeedActionResponse, NeedItem, QueueItem } from '../../api/types.gen';

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

// `satisfies` fails the build when the golden files lose a field or change its type; the cast only narrows
// the string unions JSON imports widen.
export const goHome = (goHomeJson satisfies Wire<Home>) as Home;
export const goHistory = (goHistoryJson satisfies Wire<HistoryResponse>) as HistoryResponse;
export const goNeedAction = (goNeedActionJson satisfies Wire<NeedActionResponse>) as NeedActionResponse;

const approval: NeedItem = {
  id: 'messages/2026-09-28/0001-engineering-lead.md',
  kind: 'approval',
  title: 'Approve $1,840 for a year of hosting',
  from: { slug: 'engineering-lead', name: 'Engineering lead' },
  at: '2026-09-20T16:52:00Z',
  body_md: 'Hosting quote is in: **$92** a month, billed yearly.',
  attachments: [{ name: 'hosting-quote.pdf', size_bytes: 86016, url: '/attachments/3f2a' }],
  raw_url: '/messages/2026-09-28/0001-engineering-lead.md',
};

const notice: QueueItem = {
  path: 'messages/2026-09-28/0009-chief-of-staff.md',
  kind: 'notice',
  title: 'Hold pricing page copy changes until Friday',
  from: { slug: 'chief-of-staff', name: 'Chief of Staff' },
  to: [{ slug: 'support-lead', name: 'Support lead' }],
  body_md: 'Keep drafting, but nothing ships before Friday.',
  attachments: [],
  queued_at: '2026-09-20T17:00:00Z',
  releases_at: '2026-09-20T17:01:05Z',
  held: false,
  raw_url: '/messages/2026-09-28/0009-chief-of-staff.md',
};

/** Busy: every kind of need and three queued messages, one held. */
export const busyHome: Home = {
  ...goHome,
  needs: [approval, ...goHome.needs],
  queue: [...goHome.queue, notice],
};

const quietGoal: Goal = { id: 40, title: 'Ship the release', owner: 'engineering-lead', done: 9, total: 12, blocked: [], workers: ['buyer'] };

/** Quiet: nothing needs you; two messages count down. */
export const quietHome: Home = {
  goals: [quietGoal],
  readouts: { working: 1, blocked: 0, closed_week: 9, spend_today: 6.1, spend_7d: 78.3 },
  needs: [],
  queue: goHome.queue.filter((q) => !q.held).concat(notice),
  auto_release: '30s',
  history_total: 41,
};

/** Empty: nothing needs you and nothing is queued. */
export const emptyHome: Home = {
  goals: [],
  readouts: { working: 0, blocked: 0, closed_week: 0, spend_today: 0, spend_7d: 0 },
  needs: [],
  queue: [],
  auto_release: '30s',
  history_total: 0,
};
