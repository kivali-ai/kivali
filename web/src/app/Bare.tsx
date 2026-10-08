import type { ReactNode } from 'react';
import { cx } from '../lib/cx';

// Copied from web/public into the build; the server serves them without a session (ui.PublicDirs).
const LOGO_BASE = '/logos/';

/** The Kivali lockup, small. The dark variant swaps in under the dark theme (see frame.css). */
export function KivaliLockup() {
  return (
    <span className="app-lockup">
      <img className="app-lockup-light" src={LOGO_BASE + 'kivali-lockup.svg'} alt="Kivali" />
      <img className="app-lockup-dark" src={LOGO_BASE + 'kivali-lockup-dark.svg'} alt="" aria-hidden="true" />
    </span>
  );
}

export interface BareProps {
  /** Column width: `narrow` for sign-in (360), `medium` for 404 and setup steps (480), `wide` (560). */
  width?: 'narrow' | 'medium' | 'wide';
  /** Show the Kivali lockup at the foot. Default true. */
  foot?: boolean;
  children: ReactNode;
}

/** The page outside the frame: one centered column, the Kivali lockup at the foot. Sign-in, 404 and setup use it. */
export function Bare({ width = 'medium', foot = true, children }: BareProps) {
  return (
    <div className="app-bare">
      <main className="app-bare-main">
        <div className={cx('app-bare-column', 'is-' + width)}>{children}</div>
      </main>
      {foot && (
        <footer className="app-bare-foot">
          <KivaliLockup />
        </footer>
      )}
    </div>
  );
}
