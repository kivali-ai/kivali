import type { SoftwarePackage } from '../../api/types.gen';
import { Text } from '../../ds';
import '../../styles/software-packages.css';

/**
 * The packages an Install software request asks for, one per row: the package name first, then the version
 * Debian has and its one-line description when Debian's package list has them. A virtual name says which
 * package it installs. Shared by the Needs-you row and the proposal page.
 */
export function SoftwarePackages({ packages }: { packages: SoftwarePackage[] }) {
  return (
    <ul className="app-software-packages" aria-label="Software to install">
      {packages.map((p) => (
        <li key={p.name} className="app-software-package">
          <div className="app-software-package-head">
            <Text variant="code" className="app-software-package-name">
              {p.name}
            </Text>
            {p.version && (
              <Text variant="label" tone="muted">
                {`Version ${p.version}`}
              </Text>
            )}
          </div>
          {p.synopsis && (
            <Text as="p" variant="caption" tone="muted">
              {p.synopsis}
            </Text>
          )}
          {p.provided_by && (
            <Text as="p" variant="caption" tone="muted">
              {`Installs ${p.provided_by}`}
            </Text>
          )}
        </li>
      ))}
    </ul>
  );
}
