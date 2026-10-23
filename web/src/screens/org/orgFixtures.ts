// Org fixtures: the Go golden files, checked against the generated types with `satisfies` so a wire change
// fails the TypeScript build.
import goAccessChoicesJson from '../../../../internal/web/apitypes/testdata/access_choices.json';
import goAccessWriteJson from '../../../../internal/web/apitypes/testdata/access_write.json';
import goConnectionWriteJson from '../../../../internal/web/apitypes/testdata/connection_write.json';
import goHandbookJson from '../../../../internal/web/apitypes/testdata/handbook.json';
import goIntegrationAuditJson from '../../../../internal/web/apitypes/testdata/integration_audit.json';
import goIntegrationLogsJson from '../../../../internal/web/apitypes/testdata/integration_logs.json';
import goConnectionCreateJson from '../../../../internal/web/apitypes/testdata/connection_create.json';
import goBrowserSignInJson from '../../../../internal/web/apitypes/testdata/browser_sign_in.json';
import goDeviceSignInJson from '../../../../internal/web/apitypes/testdata/device_sign_in.json';
import goIntegrationPageJson from '../../../../internal/web/apitypes/testdata/integration_page.json';
import goIntegrationPageMultipleJson from '../../../../internal/web/apitypes/testdata/integration_page_multiple.json';
import goIntegrationPageSignInJson from '../../../../internal/web/apitypes/testdata/integration_page_sign_in.json';
import goIntegrationPageSignInAgainJson from '../../../../internal/web/apitypes/testdata/integration_page_sign_in_again.json';
import goIntegrationPageSignInElsewhereJson from '../../../../internal/web/apitypes/testdata/integration_page_sign_in_elsewhere.json';
import goIntegrationUploadRefusedJson from '../../../../internal/web/apitypes/testdata/integration_upload_refused.json';
import goIntegrationVersionJson from '../../../../internal/web/apitypes/testdata/integration_version.json';
import goIntegrationVersionBuiltinJson from '../../../../internal/web/apitypes/testdata/integration_version_builtin.json';
import goIntegrationVersionShippedJson from '../../../../internal/web/apitypes/testdata/integration_version_shipped.json';
import goIntegrationVersionSignInJson from '../../../../internal/web/apitypes/testdata/integration_version_sign_in.json';
import goSignInProgressJson from '../../../../internal/web/apitypes/testdata/sign_in_progress.json';
import goSignInProgressFailedJson from '../../../../internal/web/apitypes/testdata/sign_in_progress_failed.json';
import goIntegrationsJson from '../../../../internal/web/apitypes/testdata/integrations.json';
import goNetworkJson from '../../../../internal/web/apitypes/testdata/network.json';
import goOrgJson from '../../../../internal/web/apitypes/testdata/org.json';
import goOrgSoftwareJson from '../../../../internal/web/apitypes/testdata/org_software.json';
import goUsageJson from '../../../../internal/web/apitypes/testdata/org_usage.json';
import goFilesJson from '../../../../internal/web/apitypes/testdata/project_files.json';
import goDowngradeJson from '../../../../internal/web/apitypes/testdata/skill_downgrade.json';
import goSkillsJson from '../../../../internal/web/apitypes/testdata/skills.json';
import goSoftwareAddJson from '../../../../internal/web/apitypes/testdata/software_add.json';
import type {
  AccessChoices,
  AccessWrite,
  BrowserSignIn,
  ConnectionWrite,
  DeviceSignIn,
  Handbook,
  IntegrationAudit,
  IntegrationLogs,
  IntegrationPage,
  IntegrationUploadRefused,
  IntegrationVersion,
  Integrations,
  Network,
  Org,
  OrgSoftware,
  ProjectFiles,
  SignInProgress,
  SkillDowngrade,
  Skills,
  SoftwareAdd,
  Usage,
} from '../../api/types.gen';

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
export const goOrgSoftware = (goOrgSoftwareJson satisfies Wire<OrgSoftware>) as OrgSoftware;
export const goSoftwareAdd = (goSoftwareAddJson satisfies Wire<SoftwareAdd>) as SoftwareAdd;
export const goNetwork =(goNetworkJson satisfies Wire<Network>) as Network;
export const goIntegrations = (goIntegrationsJson satisfies Wire<Integrations>) as Integrations;
export const goIntegrationPage = (goIntegrationPageJson satisfies Wire<IntegrationPage>) as IntegrationPage;
/** Git's page: two connections, GitHub and a company GitLab, with one agent's access on each. */
export const goIntegrationPageMultiple = (goIntegrationPageMultipleJson satisfies Wire<IntegrationPage>) as IntegrationPage;
/** A GitHub bundle's page, connected by signing in, its token refreshed before it expires. */
export const goIntegrationPageSignIn = (goIntegrationPageSignInJson satisfies Wire<IntegrationPage>) as IntegrationPage;
/** The same connection after the vendor refused a refresh: the owner must sign in again. */
export const goIntegrationPageSignInAgain = (goIntegrationPageSignInAgainJson satisfies Wire<IntegrationPage>) as IntegrationPage;
/** The same page as Kivali Desktop's window for a team elsewhere reads it: browser sign-in alone, which cannot finish there. */
export const goIntegrationPageSignInElsewhere = (goIntegrationPageSignInElsewhereJson satisfies Wire<IntegrationPage>) as IntegrationPage;
/** A browser sign-in just started: the service's sign-in page to send the browser to. */
export const goBrowserSignIn = (goBrowserSignInJson satisfies Wire<BrowserSignIn>) as BrowserSignIn;
/** A device sign-in just started: the code, the service's page, and when the code stops working. */
export const goDeviceSignIn = (goDeviceSignInJson satisfies Wire<DeviceSignIn>) as DeviceSignIn;
/** A sign-in still waiting for the owner's approval. */
export const goSignInProgress = (goSignInProgressJson satisfies Wire<SignInProgress>) as SignInProgress;
/** A sign-in the owner turned down on the service's page, with the service's words. */
export const goSignInProgressFailed = (goSignInProgressFailedJson satisfies Wire<SignInProgress>) as SignInProgress;
export const goIntegrationVersion = (goIntegrationVersionJson satisfies Wire<IntegrationVersion>) as IntegrationVersion;
/** A version of an integration that ships with Kivali. */
export const goIntegrationVersionBuiltin = (goIntegrationVersionBuiltinJson satisfies Wire<IntegrationVersion>) as IntegrationVersion;
/** A version of an integration that ships with Kivali as a bundle: its runtime, hosts and bundle are an upload's. */
export const goIntegrationVersionShipped = (goIntegrationVersionShippedJson satisfies Wire<IntegrationVersion>) as IntegrationVersion;
/** The install screen of a bundle that connects by signing in in the browser: its scopes and sign-in hosts. */
export const goIntegrationVersionSignIn = (goIntegrationVersionSignInJson satisfies Wire<IntegrationVersion>) as IntegrationVersion;
export const goIntegrationUploadRefused = (goIntegrationUploadRefusedJson satisfies Wire<IntegrationUploadRefused>) as IntegrationUploadRefused;
export const goIntegrationLogs = (goIntegrationLogsJson satisfies Wire<IntegrationLogs>) as IntegrationLogs;
export const goIntegrationAudit = (goIntegrationAuditJson satisfies Wire<IntegrationAudit>) as IntegrationAudit;
/** Request bodies and the options answer, as the Go server's goldens have them. */
export const goConnectionWrite = (goConnectionWriteJson satisfies Wire<ConnectionWrite>) as ConnectionWrite;
/** A new connection with its label, as POST .../connections reads it. */
export const goConnectionCreate = (goConnectionCreateJson satisfies Wire<ConnectionWrite>) as ConnectionWrite;
export const goAccessWrite = (goAccessWriteJson satisfies Wire<AccessWrite>) as AccessWrite;
export const goAccessChoices = (goAccessChoicesJson satisfies Wire<AccessChoices>) as AccessChoices;
