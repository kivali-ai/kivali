// Org fixtures: the Go golden files, checked against the generated types with `satisfies` so a wire change
// fails the TypeScript build.
import goHandbookJson from '../../../../internal/web/apitypes/testdata/handbook.json';
import goNetworkJson from '../../../../internal/web/apitypes/testdata/network.json';
import goOrgJson from '../../../../internal/web/apitypes/testdata/org.json';
import goUsageJson from '../../../../internal/web/apitypes/testdata/org_usage.json';
import goFilesJson from '../../../../internal/web/apitypes/testdata/project_files.json';
import goDowngradeJson from '../../../../internal/web/apitypes/testdata/skill_downgrade.json';
import goSkillsJson from '../../../../internal/web/apitypes/testdata/skills.json';
import type { Handbook, Network, Org, ProjectFiles, SkillDowngrade, Skills, Usage } from '../../api/types.gen';

/** The generated type with every string union widened to string: what a JSON import can be checked against. */
type Wire<T> = T extends string
  ? string
  : T extends number
    ? number
    : T extends boolean
      ? boolean
      : T extends readonly (infer U)[]
        ? Wire<U>[]
        : T extends object
          ? { [K in keyof T]: Wire<T[K]> }
          : T;

export const goUsage = (goUsageJson satisfies Wire<Usage>) as Usage;
export const goOrg = (goOrgJson satisfies Wire<Org>) as Org;
export const goHandbook = (goHandbookJson satisfies Wire<Handbook>) as Handbook;
export const goFiles = (goFilesJson satisfies Wire<ProjectFiles>) as ProjectFiles;
export const goSkills = (goSkillsJson satisfies Wire<Skills>) as Skills;
export const goDowngrade = (goDowngradeJson satisfies Wire<SkillDowngrade>) as SkillDowngrade;
export const goNetwork = (goNetworkJson satisfies Wire<Network>) as Network;
