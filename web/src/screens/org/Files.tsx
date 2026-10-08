import { useState } from 'react';
import { apiDelete, apiGetBlob, apiPost, apiPostForm } from '../../api/client';
import type { FilesBulkDelete, ProjectFile, ProjectFiles } from '../../api/types.gen';
import { Badge, Button, Checkbox, FileDrop, Icon, ListRow } from '../../ds';
import { shortDate } from '../../lib/time';
import { ErrorBanner, None, RowList, RowSkeletons, SectionHead, formatBytes, saveFile } from './parts';
import type { SectionProps } from './sections';
import { useAction, useResource } from './useResource';

const PATH = '/api/v1/org/files';

/** "38 files · 64 MB" */
function filesMeta(files: ProjectFile[]): string {
  const n = files.length;
  const bytes = files.reduce((sum, f) => sum + f.size_bytes, 0);
  return `${n} ${n === 1 ? 'file' : 'files'} · ${formatBytes(bytes)}`;
}

export function FilesSection({ showHead }: SectionProps) {
  const list = useResource<ProjectFiles>(PATH);
  const action = useAction();
  const [picked, setPicked] = useState<ReadonlySet<string>>(new Set());

  const apply = (next: ProjectFiles) => {
    list.set(next);
    const live = new Set(next.files.map((f) => f.sha));
    setPicked((prev) => new Set([...prev].filter((sha) => live.has(sha))));
  };
  const upload = (files: File[]) => {
    if (files.length === 0) return;
    void action.run(async () => {
      const form = new FormData();
      for (const f of files) form.append('files[]', f);
      apply(await apiPostForm<ProjectFiles>(PATH, form));
    });
  };
  const download = (f: ProjectFile) =>
    action.run(async () => {
      const file = await apiGetBlob(f.url);
      saveFile(file.blob, file.filename ?? f.name);
    });
  const remove = (f: ProjectFile) =>
    action.run(async () => {
      apply(await apiDelete<ProjectFiles>(PATH + '/' + encodeURIComponent(f.sha)));
    });
  const removePicked = () =>
    action.run(async () => {
      const body: FilesBulkDelete = { shas: [...picked] };
      apply(await apiPost<ProjectFiles>(PATH + '/bulk-delete', body));
    });
  const toggle = (sha: string, on: boolean) =>
    setPicked((prev) => {
      const next = new Set(prev);
      if (on) next.add(sha);
      else next.delete(sha);
      return next;
    });

  const now = new Date();
  return (
    <>
      <SectionHead title="Project files" show={showHead} meta={list.data ? filesMeta(list.data.files) : undefined} />
      <ErrorBanner error={list.error ?? action.error} {...(action.error ? { onDismiss: action.dismiss } : {})} />
      <FileDrop label="Drop project files" hint="Plans, briefs, specs. Agents read them when they need background." onFiles={upload} />
      {!list.data && !list.error && <RowSkeletons />}
      {list.data && list.data.files.length === 0 && <None>No project files yet. Add the documents your agents should know about.</None>}
      {list.data && list.data.files.length > 0 && (
        <>
          {picked.size > 0 && (
            <div className="app-org-actions">
              <Button variant="ghost" size="sm" onClick={() => void removePicked()} disabled={action.busy}>
                Remove {picked.size} selected
              </Button>
            </div>
          )}
          <RowList>
            {list.data.files.map((f) => (
              <ListRow
                key={f.sha}
                lead={<Checkbox label="" ariaLabel={`Select ${f.name}`} checked={picked.has(f.sha)} onCheckedChange={(v) => toggle(f.sha, v === true)} />}
                title={f.name}
                meta={
                  <>
                    {formatBytes(f.size_bytes)} · uploaded {shortDate(new Date(f.uploaded_at), now)}{' '}
                    <Badge>{f.extracted ? 'Text extracted' : 'Stored only'}</Badge>
                  </>
                }
                trail={
                  <>
                    <Button variant="ghost" size="sm" icon={<Icon name="download" />} aria-label={`Download ${f.name}`} onClick={() => void download(f)} disabled={action.busy}>
                      Download
                    </Button>
                    <Button variant="ghost" size="sm" aria-label={`Remove ${f.name}`} onClick={() => void remove(f)} disabled={action.busy}>
                      Remove
                    </Button>
                  </>
                }
              />
            ))}
          </RowList>
        </>
      )}
    </>
  );
}
