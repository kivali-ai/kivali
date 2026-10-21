import { useState } from 'react';
import { ApiError, apiGet, apiPut } from '../../api/client';
import type { AccessChoices, AccessField, AccessForm, AccessWrite, IntegrationPage, PersonRef } from '../../api/types.gen';
import { Banner, Button, Checkbox, Dialog, DialogClose, Icon, Select, Text, TextField } from '../../ds';
import { integrationApi } from './integrationParts';
import { AddField } from './Lists';
import { asApiError } from './useResource';

/** What one field of the form holds while it is being filled. */
type Value = string | boolean | string[];
type Values = Record<string, Value>;

function asStrings(v: unknown): string[] {
  return Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string') : [];
}

/** The form's starting values: the agent's current access where it fits a field, else empty. */
export function initialValues(form: AccessForm | undefined, access: unknown): Values {
  const given = access !== null && typeof access === 'object' && !Array.isArray(access) ? (access as Record<string, unknown>) : {};
  const out: Values = {};
  for (const f of form?.fields ?? []) {
    const v = given[f.key];
    switch (f.kind) {
      case 'switch':
        out[f.key] = v === true;
        break;
      case 'list':
      case 'choices':
        out[f.key] = asStrings(v);
        break;
      default:
        out[f.key] = typeof v === 'string' ? v : '';
    }
  }
  return out;
}

/** The filled form as the integration reads it: switches always, other fields only when they hold something. */
export function accessBody(form: AccessForm | undefined, values: Values): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const f of form?.fields ?? []) {
    const v = values[f.key];
    switch (f.kind) {
      case 'switch':
        out[f.key] = v === true;
        break;
      case 'list':
      case 'choices':
        if (Array.isArray(v) && v.length > 0) out[f.key] = v;
        break;
      default:
        if (typeof v === 'string' && v.trim() !== '') out[f.key] = v.trim();
    }
  }
  return out;
}

/** A field's label: optional fields say so, as every form in Kivali does. A switch is never optional. */
function fieldLabel(f: AccessField): string {
  return f.required || f.kind === 'switch' ? f.title : f.title + ' (optional)';
}

/** Every option a field can show: the bundle's, the ones the integration listed, then any already chosen. */
function optionsOf(f: AccessField, loaded: readonly string[], chosen: readonly string[]): string[] {
  const out: string[] = [];
  for (const o of [...f.options, ...loaded, ...chosen]) if (o && !out.includes(o)) out.push(o);
  return out;
}

interface LoadState {
  options: string[];
  busy: boolean;
  error: string | null;
}

interface FieldProps {
  field: AccessField;
  value: Value | undefined;
  onChange(v: Value): void;
  integration: string;
  load: LoadState | undefined;
  onLoad(): void;
}

/** "Load options": asks the integration, through the connection, for the options it lists for the field. */
function LoadOptions({ field, integration, load, onLoad }: Pick<FieldProps, 'field' | 'integration' | 'load' | 'onLoad'>) {
  if (!field.choices) return null;
  return (
    <div className="app-int-load">
      <Button variant="ghost" size="sm" icon={<Icon name="rotate-ccw" />} loading={load?.busy ?? false} disabled={load?.busy ?? false} onClick={onLoad}>
        Load options
      </Button>
      {load?.error ? (
        <Text variant="caption" tone="danger" role="alert">
          {load.error}
        </Text>
      ) : (
        !load && (
          <Text variant="caption" tone="muted">
            {integration + ' lists these itself.'}
          </Text>
        )
      )}
    </div>
  );
}

function AccessFieldInput({ field, value, onChange, integration, load, onLoad }: FieldProps) {
  const hint = field.description || undefined;
  switch (field.kind) {
    case 'switch':
      return <Checkbox label={field.title} {...(hint ? { hint } : {})} checked={value === true} onCheckedChange={(v) => onChange(v === true)} />;
    case 'choice': {
      const current = typeof value === 'string' ? value : '';
      const options = optionsOf(field, load?.options ?? [], current ? [current] : []);
      return (
        <div className="app-int-field">
          <Select
            label={fieldLabel(field)}
            {...(hint ? { hint } : {})}
            value={current}
            onChange={(e) => onChange(e.currentTarget.value)}
            options={[{ value: '', label: 'Not set' }, ...options.map((o) => ({ value: o, label: o }))]}
          />
          <LoadOptions field={field} integration={integration} load={load} onLoad={onLoad} />
        </div>
      );
    }
    case 'choices': {
      const chosen = asStrings(value);
      const options = optionsOf(field, load?.options ?? [], chosen);
      return (
        <fieldset className="app-int-fieldset">
          <legend className="app-int-legend">{fieldLabel(field)}</legend>
          {hint && (
            <Text as="p" variant="caption" tone="muted">
              {hint}
            </Text>
          )}
          {options.length === 0 && (
            <Text as="p" variant="caption" tone="muted">
              No options yet.
            </Text>
          )}
          {options.map((o) => (
            <Checkbox
              key={o}
              label={o}
              checked={chosen.includes(o)}
              onCheckedChange={(on) => onChange(on === true ? [...chosen, o] : chosen.filter((c) => c !== o))}
            />
          ))}
          <LoadOptions field={field} integration={integration} load={load} onLoad={onLoad} />
        </fieldset>
      );
    }
    case 'list': {
      const entries = asStrings(value);
      return (
        <div className="app-int-field">
          <AddField
            label={fieldLabel(field)}
            placeholder=""
            button="Add"
            busy={false}
            onAdd={(v) => {
              if (!entries.includes(v)) onChange([...entries, v]);
              return Promise.resolve(true);
            }}
          />
          {hint && (
            <Text as="p" variant="caption" tone="muted">
              {hint}
            </Text>
          )}
          {entries.length > 0 && (
            <ul className="app-int-chips" aria-label={field.title}>
              {entries.map((e) => (
                <li key={e}>
                  <Button size="sm" variant="secondary" icon={<Icon name="x" />} aria-label={'Remove ' + e} onClick={() => onChange(entries.filter((x) => x !== e))}>
                    {e}
                  </Button>
                </li>
              ))}
            </ul>
          )}
        </div>
      );
    }
    default:
      return (
        <TextField label={fieldLabel(field)} {...(hint ? { hint } : {})} value={typeof value === 'string' ? value : ''} onChange={(e) => onChange(e.currentTarget.value)} />
      );
  }
}

export interface AccessDialogProps {
  /** The integration's name, for its API paths, and the name people read. */
  name: string;
  displayName: string;
  agent: PersonRef;
  form: AccessForm | undefined;
  /** The agent's access as it stands, when it has some. */
  access?: unknown;
  onClose(): void;
  onSaved(page: IntegrationPage): void;
}

/**
 * One agent's access to an integration: the form the integration describes, filled in, or a plain yes for an
 * integration whose access is all or nothing. Saving replaces the agent's access with what the form holds.
 */
export function AccessDialog({ name, displayName, agent, form, access, onClose, onSaved }: AccessDialogProps) {
  const [values, setValues] = useState<Values>(() => initialValues(form, access));
  const [loads, setLoads] = useState<Record<string, LoadState>>({});
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<ApiError | null>(null);
  const fields = form?.fields ?? [];
  const changing = access !== undefined;

  const load = async (key: string) => {
    setLoads((m) => ({ ...m, [key]: { options: m[key]?.options ?? [], busy: true, error: null } }));
    try {
      const res = await apiGet<AccessChoices>(integrationApi(name) + '/choices/' + encodeURIComponent(key));
      setLoads((m) => ({ ...m, [key]: { options: res.options, busy: false, error: null } }));
    } catch (err) {
      setLoads((m) => ({ ...m, [key]: { options: m[key]?.options ?? [], busy: false, error: asApiError(err).message } }));
    }
  };

  const save = async () => {
    setBusy(true);
    setError(null);
    try {
      const body: AccessWrite = { access: accessBody(form, values) };
      const next = await apiPut<IntegrationPage>(integrationApi(name) + '/access/' + encodeURIComponent(agent.slug), body);
      onSaved(next);
    } catch (err) {
      setError(asApiError(err));
      setBusy(false);
    }
  };

  return (
    <Dialog
      open
      onOpenChange={(o) => {
        if (!o) onClose();
      }}
      title={(changing ? 'Change ' + agent.name + '’s access to ' : 'Give ' + agent.name + ' access to ') + displayName}
      {...(fields.length === 0 ? { description: 'This integration’s access is all or nothing.' } : {})}
      footer={
        <>
          <DialogClose>
            <Button variant="secondary">Cancel</Button>
          </DialogClose>
          <Button variant="primary" loading={busy} disabled={busy} onClick={() => void save()}>
            {fields.length === 0 ? 'Approve' : changing ? 'Save access' : 'Give access'}
          </Button>
        </>
      }
    >
      {(error || fields.length > 0) && (
        <div className="app-int-form">
          {error && (
            <Banner tone="danger" title={error.message}>
              {error.who === 'you' ? 'Change the form, then try again.' : error.who}
            </Banner>
          )}
          {fields.map((f) => (
            <AccessFieldInput
              key={f.key}
              field={f}
              value={values[f.key]}
              onChange={(v) => setValues((m) => ({ ...m, [f.key]: v }))}
              integration={displayName}
              load={loads[f.key]}
              onLoad={() => void load(f.key)}
            />
          ))}
        </div>
      )}
    </Dialog>
  );
}
