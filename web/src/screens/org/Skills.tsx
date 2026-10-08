import { useState } from 'react';
import { ApiError, apiDelete, apiPost, apiPostForm } from '../../api/client';
import type { Skill, SkillDowngrade, Skills } from '../../api/types.gen';
import { Badge, Button, Dialog, DialogClose, Icon, ListRow, Switch } from '../../ds';
import { pickFiles } from '../../lib/pickFiles';
import { ErrorBanner, None, RowList, RowSkeletons, SectionHead } from './parts';
import type { SectionProps } from './sections';
import { useAction, useResource } from './useResource';

const PATH = '/api/v1/org/skills';

/** A 409 whose body names both versions is a downgrade the person may confirm; any other refusal is an error. */
export function downgradeOf(err: unknown): SkillDowngrade | null {
  if (!(err instanceof ApiError) || err.status !== 409) return null;
  const b = err.body as Partial<SkillDowngrade> | null | undefined;
  if (!b || typeof b.old_version !== 'string' || typeof b.new_version !== 'string') return null;
  return { error: err.message, who: err.who, old_version: b.old_version, new_version: b.new_version };
}

/** "9 installed · 1 off" */
function skillsMeta(skills: Skill[]): string {
  const off = skills.filter((s) => !s.enabled).length;
  return `${skills.length} installed` + (off > 0 ? ` · ${off} off` : '');
}

/** "v2.1 · Read and fill PDFs." */
function skillLine(s: Skill): string {
  const version = s.version ? 'v' + s.version.replace(/^v/, '') : '';
  return [version, s.description].filter(Boolean).join(' · ');
}

interface Downgrade {
  file: File;
  info: SkillDowngrade;
}

export function SkillsSection({ showHead }: SectionProps) {
  const list = useResource<Skills>(PATH);
  const action = useAction();
  const [downgrade, setDowngrade] = useState<Downgrade | null>(null);
  const [removing, setRemoving] = useState<Skill | null>(null);

  const send = (file: File, confirm: boolean) =>
    action.run(async () => {
      const form = new FormData();
      form.append('file', file);
      if (confirm) form.append('confirm_downgrade', 'true');
      let next: Skills;
      try {
        next = await apiPostForm<Skills>(PATH, form);
      } catch (err) {
        const info = confirm ? null : downgradeOf(err);
        if (!info) throw err;
        setDowngrade({ file, info });
        return;
      }
      setDowngrade(null);
      list.set(next);
    });
  const upload = async () => {
    const [file] = await pickFiles({ multiple: false });
    if (file) await send(file, false);
  };
  const toggle = (s: Skill, on: boolean) =>
    action.run(async () => {
      list.set(await apiPost<Skills>(PATH + '/' + encodeURIComponent(s.name) + (on ? '/enable' : '/disable')));
    });
  const remove = (s: Skill) =>
    action.run(async () => {
      list.set(await apiDelete<Skills>(PATH + '/' + encodeURIComponent(s.name)));
      setRemoving(null);
    });

  return (
    <>
      <SectionHead title="Skills" show={showHead} meta={list.data ? skillsMeta(list.data.skills) : undefined} />
      <ErrorBanner error={list.error ?? action.error} {...(action.error ? { onDismiss: action.dismiss } : {})} />
      {!list.data && !list.error && <RowSkeletons />}
      {list.data && list.data.skills.length === 0 && <None>No skills installed.</None>}
      {list.data && list.data.skills.length > 0 && (
        <RowList>
          {list.data.skills.map((s) => (
            <ListRow
              key={s.name}
              lead={<Icon name="puzzle" size={20} />}
              title={s.name}
              meta={
                <>
                  {s.builtin && <Badge variant="outline">Built in</Badge>} {skillLine(s)}
                </>
              }
              trail={
                <>
                  {!s.builtin && (
                    <Button variant="ghost" size="sm" aria-label={`Remove ${s.name}`} onClick={() => setRemoving(s)} disabled={action.busy}>
                      Remove
                    </Button>
                  )}
                  <Switch className="app-org-switch" label={`Enable ${s.name}`} checked={s.enabled} disabled={action.busy} onCheckedChange={(on) => void toggle(s, on)} />
                </>
              }
            />
          ))}
        </RowList>
      )}
      <div className="app-org-actions">
        <Button variant="secondary" icon={<Icon name="upload" />} onClick={() => void upload()} disabled={action.busy}>
          Upload a skill
        </Button>
      </div>

      <Dialog
        open={downgrade !== null}
        onOpenChange={(o) => {
          if (!o) setDowngrade(null);
        }}
        title={downgrade ? `Replace version ${downgrade.info.old_version} with older version ${downgrade.info.new_version}?` : ''}
        description={downgrade ? downgrade.info.error : undefined}
        footer={
          <>
            <DialogClose>
              <Button variant="secondary">Cancel</Button>
            </DialogClose>
            <Button variant="primary" onClick={() => downgrade && void send(downgrade.file, true)} disabled={action.busy}>
              Replace
            </Button>
          </>
        }
      />
      <Dialog
        open={removing !== null}
        onOpenChange={(o) => {
          if (!o) setRemoving(null);
        }}
        tone="danger"
        title={removing ? `Remove ${removing.name}?` : ''}
        description="Agents lose this skill straight away. You can add it again later."
        footer={
          <>
            <DialogClose>
              <Button variant="secondary">Cancel</Button>
            </DialogClose>
            <Button variant="danger" onClick={() => removing && void remove(removing)} disabled={action.busy}>
              Remove
            </Button>
          </>
        }
      />
    </>
  );
}
