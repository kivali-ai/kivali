import type { Setup, SetupCredential } from '../../api/types.gen';
import { Text } from '../../ds';
import { useOrg } from '../../state/OrgProvider';
import { SectionHead } from './parts';
import type { SectionProps } from './sections';
import { useResource } from './useResource';

/** "Kivali 0.42.1" from /api/v1/me's "v0.42.1". */
export function versionLabel(version: string | undefined): string {
  const v = (version ?? '').replace(/^v(?=\d)/, '');
  return v ? 'Kivali ' + v : 'Kivali';
}

/** What model calls are billed to, from /api/v1/setup's credential: "Claude Max", "Amazon Bedrock", or that nothing is signed in. */
export function credentialLine(credential: SetupCredential | undefined): string {
  if (!credential) return '';
  if (!credential.present) return 'Not signed in to Claude';
  return credential.billing || 'Signed in to Claude';
}

/** "Kivali 0.42.1 · Claude Max": the version from /api/v1/me, then credentialLine. */
export function aboutLine(version: string | undefined, credential: SetupCredential | undefined): string {
  const parts = [versionLabel(version)];
  const c = credentialLine(credential);
  if (c) parts.push(c);
  return parts.join(' · ');
}

export function AboutSection({ showHead }: SectionProps) {
  const { me } = useOrg();
  // Only the credential is read from setup; if it fails the line just leaves it out.
  const setup = useResource<Setup>('/api/v1/setup');
  return (
    <>
      <SectionHead title="About" show={showHead} />
      <Text as="p" variant="caption" tone="muted" className="app-org-plain">
        {aboutLine(me?.version, setup.data?.credential)}
      </Text>
    </>
  );
}
