import { useState } from 'react';
import { Button, FileDrop, OrgMark, Text } from '../../ds';
import { pickFiles } from '../../lib/pickFiles';

export interface LogoFieldProps {
  /** The org's name; the logo's accessible name is "<name> logo". */
  name: string;
  /** The uploaded logo's URL; absent means no logo is set. */
  src?: string | undefined;
  accept: string;
  /** The accepted format, one line. Shown under the actions, or in the drop zone when there is no logo. */
  hint: string;
  disabled?: boolean;
  onFiles(files: File[]): void;
  /** Absent when the server has no way to remove a logo here. */
  onRemove?: () => void;
}

/**
 * The logo control shared by Org > Organization and the setup wizard's "Your org" step.
 * Without a logo: the monogram and a drop zone. With one: the logo leads, large, with Replace and Remove
 * beside it and no drop zone; dropping an image on the tile still replaces it.
 */
export function LogoField({ name, src, accept, hint, disabled, onFiles, onRemove }: LogoFieldProps) {
  const [over, setOver] = useState(false);

  if (!src) {
    return (
      <div className="app-logo-empty">
        <OrgMark name={name} size={40} />
        <div className="app-logo-drop">
          <FileDrop label="Drop a square logo" hint={hint} multiple={false} accept={accept} onFiles={onFiles} />
        </div>
      </div>
    );
  }

  const replace = () => {
    void pickFiles({ multiple: false, accept }).then((files) => {
      if (files.length > 0) onFiles(files);
    });
  };

  return (
    <div className="app-logo-set">
      <div
        className={over ? 'app-logo-tile is-over' : 'app-logo-tile'}
        data-testid="logo-tile"
        onDragEnter={(e) => {
          e.preventDefault();
          setOver(true);
        }}
        onDragOver={(e) => {
          e.preventDefault();
          setOver(true);
        }}
        onDragLeave={() => setOver(false)}
        onDrop={(e) => {
          e.preventDefault();
          setOver(false);
          onFiles(Array.from(e.dataTransfer.files));
        }}
      >
        <OrgMark name={name + ' logo'} size={56} src={src} />
      </div>
      <div className="app-logo-side">
        <div className="app-logo-actions">
          <Button variant="secondary" size="sm" onClick={replace} disabled={disabled}>
            Replace
          </Button>
          {onRemove && (
            <Button variant="ghost" size="sm" onClick={onRemove} disabled={disabled}>
              Remove
            </Button>
          )}
        </div>
        <Text as="p" variant="caption" tone="muted" className="app-logo-caption">
          {hint}
        </Text>
      </div>
    </div>
  );
}
