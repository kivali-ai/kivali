import { useId } from 'react';
import type { ReactNode } from 'react';
import { cx } from './cx';
import { Icon } from './Icon/Icon';

/** A stable element id: the given one, or a generated `kv-` id. */
export function useFieldId(id?: string): string {
  const generated = useId().replace(/:/g, '');
  return id ?? 'kv-' + generated;
}

interface FieldProps {
  id: string;
  label?: string;
  hint?: string;
  error?: string;
  children: ReactNode;
}

/** Label, control and hint or error message shared by TextField and Select. */
export function Field({ id, label, hint, error, children }: FieldProps) {
  return (
    <div className={cx('kv-field', error && 'is-invalid')}>
      {label && (
        <label className="kv-field-label" htmlFor={id}>
          {label}
        </label>
      )}
      {children}
      {error ? (
        <p className="kv-field-error" id={id + '-msg'}>
          <Icon name="triangle-alert" size={14} />
          {error}
        </p>
      ) : hint ? (
        <p className="kv-field-hint" id={id + '-msg'}>
          {hint}
        </p>
      ) : null}
    </div>
  );
}
