/** Every section receives the same two flags. */
export interface SectionProps {
  /** Below 960: a sub-page on its own. */
  phone: boolean;
  /** Desktop shows each section's heading; on phone the frame header names the sub-page. */
  showHead: boolean;
}

export type SectionKey = 'usage' | 'organization' | 'handbook' | 'files' | 'skills' | 'network' | 'software' | 'backup' | 'about';

export interface SectionInfo {
  key: SectionKey;
  label: string;
  icon: string;
  /** The muted line under the title on the phone list. */
  blurb: string;
}

/** The nine sections, in page order: canvas 6a/6b's index. */
export const SECTIONS: readonly SectionInfo[] = [
  { key: 'usage', label: 'Usage', icon: 'history', blurb: 'Spend per day and who spent it' },
  { key: 'organization', label: 'Organization', icon: 'settings', blurb: 'Name and logo' },
  { key: 'handbook', label: 'Handbook', icon: 'scroll-text', blurb: 'How your team works together' },
  { key: 'files', label: 'Project files', icon: 'folder', blurb: 'Documents agents can read' },
  { key: 'skills', label: 'Skills', icon: 'puzzle', blurb: 'What agents know how to do' },
  { key: 'network', label: 'Network', icon: 'globe', blurb: 'Hosts agent shells can reach' },
  { key: 'software', label: 'Software', icon: 'download', blurb: 'Programs installed for every agent' },
  { key: 'backup', label: 'Backup and restore', icon: 'archive', blurb: 'A copy of everything, as one file' },
  { key: 'about', label: 'About', icon: 'info', blurb: 'This Kivali server' },
];

export function sectionByKey(key: string | undefined): SectionInfo | undefined {
  return SECTIONS.find((s) => s.key === key);
}
