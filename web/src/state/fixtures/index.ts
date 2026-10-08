// Recorded snapshots for tests. `goSnapshot` is the Go golden fixture, so the reducer is proven against
// what the server really emits; the others are hand-written from the generated types.
import goSnapshotJson from '../../../../internal/web/apitypes/testdata/snapshot.json';
import goMeJson from '../../../../internal/web/apitypes/testdata/me.json';
import type { Me, OrgSnapshot } from '../../api/types.gen';
import busyJson from './snapshot-busy.json';
import quietJson from './snapshot-quiet.json';
import releaseOnlyJson from './snapshot-release-only.json';

// JSON imports widen string unions to `string`; the casts are the one place that is acknowledged.
export const goSnapshot = goSnapshotJson as OrgSnapshot;
export const goMe = goMeJson as Me;
export const quietSnapshot = quietJson as OrgSnapshot;
export const busySnapshot = busyJson as OrgSnapshot;
export const releaseOnlySnapshot = releaseOnlyJson as OrgSnapshot;
