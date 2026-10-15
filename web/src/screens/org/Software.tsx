import { apiDelete, apiPost } from '../../api/client';
import type { OrgSoftware, SoftwareAdd } from '../../api/types.gen';
import { Banner, Button, Icon, ListRow, Text } from '../../ds';
import { AddField } from './Lists';
import { ErrorBanner, None, RowList, RowSkeletons, SectionHead } from './parts';
import type { SectionProps } from './sections';
import { useAction, useResource } from './useResource';

/** Package names typed or pasted into the add field: separated by spaces or commas. */
export function splitPackages(value: string): string[] {
  return value.split(/[\s,]+/).filter(Boolean);
}

interface PackageListProps {
  packages: readonly string[];
  /** Listed names that are not Debian package names: shown as not installed. */
  invalid?: readonly string[];
  /** Shown instead of the list when it is empty. */
  empty: string;
  /** Without these the list is read-only. */
  edit?: { busy: boolean; label: string; onAdd(names: string[]): Promise<boolean>; onRemove(name: string): void };
}

/** Debian package names, one per row; with `edit`, each can be removed and more can be added. */
export function PackageList({ packages, invalid = [], empty, edit }: PackageListProps) {
  return (
    <>
      {packages.length === 0 && <None>{empty}</None>}
      {packages.length > 0 && (
        <RowList>
          {packages.map((name) => (
            <ListRow
              key={name}
              lead={<Icon name="download" size={20} />}
              title={<Text variant="code">{name}</Text>}
              {...(invalid.includes(name) ? { meta: 'Not a Debian package name, so it is not installed' } : {})}
              {...(edit
                ? {
                    trail: (
                      <Button variant="ghost" size="sm" aria-label={`Remove ${name}`} onClick={() => edit.onRemove(name)} disabled={edit.busy}>
                        Remove
                      </Button>
                    ),
                  }
                : {})}
            />
          ))}
        </RowList>
      )}
      {edit && <AddField label={edit.label} placeholder="ffmpeg or poppler-utils" button="Add" busy={edit.busy} onAdd={(v) => edit.onAdd(splitPackages(v))} />}
    </>
  );
}

/** Names an add saved without checking them, because Debian's package list could not be read. */
export function UncheckedNotice({ names, onDismiss }: { names: readonly string[]; onDismiss(): void }) {
  if (names.length === 0) return null;
  return (
    <Banner tone="warning" title={`Added without checking ${names.join(', ')}`} onDismiss={onDismiss}>
      Kivali could not reach Debian’s package list to check {names.length === 1 ? 'this name' : 'these names'}. If Debian has no package by
      that name, the shell says so when it installs.
    </Banner>
  );
}

/** Org > Software: the packages every agent's shell gets. */
export function SoftwareSection({ showHead }: SectionProps) {
  const list = useResource<OrgSoftware>('/api/v1/org/software');
  const action = useAction();
  const add = (packages: string[]) =>
    action.run(async () => {
      const body: SoftwareAdd = { packages };
      list.set(await apiPost<OrgSoftware>('/api/v1/org/software', body));
    });
  const remove = (name: string) =>
    action.run(async () => {
      list.set(await apiDelete<OrgSoftware>('/api/v1/org/software/' + encodeURIComponent(name)));
    });
  const n = list.data?.packages.length;
  return (
    <>
      <SectionHead title="Software" show={showHead} meta={n === undefined ? undefined : `${n} ${n === 1 ? 'package' : 'packages'} for every agent`} />
      <Text as="p" variant="caption" tone="muted">
        Extra programs for every agent to work with, named as Debian lists them (ffmpeg, poppler-utils). Changing this list restarts the agents it affects so they can install the change, each once it has finished what it is doing. A restart takes a minute or two. An agent can also ask you to install something just for itself.
      </Text>
      <ErrorBanner error={list.error ?? action.error} {...(action.error ? { onDismiss: action.dismiss } : {})} />
      {!list.data && !list.error && <RowSkeletons />}
      {list.data && <UncheckedNotice names={list.data.unchecked} onDismiss={() => list.data && list.set({ ...list.data, unchecked: [] })} />}
      {list.data && (
        <PackageList
          packages={list.data.packages}
          invalid={list.data.invalid}
          empty="No extra software. Agents have only what Kivali already includes."
          edit={{
            busy: action.busy,
            label: 'Add software',
            onAdd: add,
            onRemove: (name) => void remove(name),
          }}
        />
      )}
    </>
  );
}
