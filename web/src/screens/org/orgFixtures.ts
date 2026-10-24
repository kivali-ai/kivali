// Org fixtures: the Go golden files, checked against the generated types with `satisfies` so a wire change
// fails the TypeScript build.
import goAccessChoicesJson from '../../../../internal/web/apitypes/testdata/access_choices.json';
import goAccessWriteJson from '../../../../internal/web/apitypes/testdata/access_write.json';
import goConnectionWriteJson from '../../../../internal/web/apitypes/testdata/connection_write.json';
import goHandbookJson from '../../../../internal/web/apitypes/testdata/handbook.json';
import goIntegrationAuditJson from '../../../../internal/web/apitypes/testdata/integration_audit.json';
import goIntegrationLogsJson from '../../../../internal/web/apitypes/testdata/integration_logs.json';
import goIntegrationPageJson from '../../../../internal/web/apitypes/testdata/integration_page.json';
import goIntegrationPageGitJson from '../../../../internal/web/apitypes/testdata/integration_page_git.json';
import goIntegrationPageSignInJson from '../../../../internal/web/apitypes/testdata/integration_page_sign_in.json';
import goIntegrationPageSignInAgainJson from '../../../../internal/web/apitypes/testdata/integration_page_sign_in_again.json';
import goIntegrationPageSignInElsewhereJson from '../../../../internal/web/apitypes/testdata/integration_page_sign_in_elsewhere.json';
import goIntegrationUploadRefusedJson from '../../../../internal/web/apitypes/testdata/integration_upload_refused.json';
import goIntegrationVersionJson from '../../../../internal/web/apitypes/testdata/integration_version.json';
import goIntegrationVersionBuiltinJson from '../../../../internal/web/apitypes/testdata/integration_version_builtin.json';
import goIntegrationVersionSignInJson from '../../../../internal/web/apitypes/testdata/integration_version_sign_in.json';
import goIntegrationVersionSetupJson from '../../../../internal/web/apitypes/testdata/integration_version_setup.json';
import goIntegrationVersionRegisteredJson from '../../../../internal/web/apitypes/testdata/integration_version_registered.json';
import goIntegrationPageSetupJson from '../../../../internal/web/apitypes/testdata/integration_page_setup.json';
import goIntegrationPageSettingUpJson from '../../../../internal/web/apitypes/testdata/integration_page_setting_up.json';
import goIntegrationPageFixJson from '../../../../internal/web/apitypes/testdata/integration_page_fix.json';
import goIntegrationPageMisfitJson from '../../../../internal/web/apitypes/testdata/integration_page_misfit.json';
import goIntegrationPageOffJson from '../../../../internal/web/apitypes/testdata/integration_page_off.json';
import goIntegrationPagePendingJson from '../../../../internal/web/apitypes/testdata/integration_page_pending.json';
import goIntegrationPageNotIncludedJson from '../../../../internal/web/apitypes/testdata/integration_page_not_included.json';
import goSetupFlowDoneJson from '../../../../internal/web/apitypes/testdata/setup_flow_done.json';
import goSetupFlowFailedJson from '../../../../internal/web/apitypes/testdata/setup_flow_failed.json';
import goSetupFlowFormJson from '../../../../internal/web/apitypes/testdata/setup_flow_form.json';
import goSetupFlowGoJson from '../../../../internal/web/apitypes/testdata/setup_flow_go.json';
import goSetupFlowGoNoReturnJson from '../../../../internal/web/apitypes/testdata/setup_flow_go_no_return.json';
import goSetupFlowRunningJson from '../../../../internal/web/apitypes/testdata/setup_flow_running.json';
import goSetupFlowWaitJson from '../../../../internal/web/apitypes/testdata/setup_flow_wait.json';
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
  ConnectionWrite,
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
  SetupFlow,
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
/** Git's page: its one connection needing its secrets again, and two agents' access, one set for a connection Git no longer has. */
export const goIntegrationPageGit = (goIntegrationPageGitJson satisfies Wire<IntegrationPage>) as IntegrationPage;
/** A GitHub bundle's page, connected by signing in, its token refreshed before it expires. */
export const goIntegrationPageSignIn = (goIntegrationPageSignInJson satisfies Wire<IntegrationPage>) as IntegrationPage;
/** The same connection after the vendor refused a refresh: the owner must sign in again. */
export const goIntegrationPageSignInAgain = (goIntegrationPageSignInAgainJson satisfies Wire<IntegrationPage>) as IntegrationPage;
/** The same page as Kivali Desktop's window for a team elsewhere reads it: browser sign-in alone, which cannot finish there. */
export const goIntegrationPageSignInElsewhere = (goIntegrationPageSignInElsewhereJson satisfies Wire<IntegrationPage>) as IntegrationPage;
/** A setup flow working out its next step, three stages of five in. */
export const goSetupFlowRunning = (goSetupFlowRunningJson satisfies Wire<SetupFlow>) as SetupFlow;
/** A setup flow asking for its declared fields, one a secret used during setup alone. */
export const goSetupFlowForm = (goSetupFlowFormJson satisfies Wire<SetupFlow>) as SetupFlow;
/** A setup flow sending the browser to the service and back. */
export const goSetupFlowGo = (goSetupFlowGoJson satisfies Wire<SetupFlow>) as SetupFlow;
/** A setup flow sending the browser to a settings page that sends it nowhere afterwards. */
export const goSetupFlowGoNoReturn = (goSetupFlowGoNoReturnJson satisfies Wire<SetupFlow>) as SetupFlow;
/** A device sign-in's flow waiting for the code to be entered on the service's page. */
export const goSetupFlowWait = (goSetupFlowWaitJson satisfies Wire<SetupFlow>) as SetupFlow;
/** A setup flow that finished. */
export const goSetupFlowDone = (goSetupFlowDoneJson satisfies Wire<SetupFlow>) as SetupFlow;
/** A setup flow the service refused, with its words. */
export const goSetupFlowFailed = (goSetupFlowFailedJson satisfies Wire<SetupFlow>) as SetupFlow;
export const goIntegrationVersion =(goIntegrationVersionJson satisfies Wire<IntegrationVersion>) as IntegrationVersion;
/** A version of an integration that ships with Kivali. */
export const goIntegrationVersionBuiltin = (goIntegrationVersionBuiltinJson satisfies Wire<IntegrationVersion>) as IntegrationVersion;
/** The install screen of a bundle that connects by signing in in the browser: its scopes and sign-in hosts. */
export const goIntegrationVersionSignIn = (goIntegrationVersionSignInJson satisfies Wire<IntegrationVersion>) as IntegrationVersion;
/** The install screen of a built-in whose connection its own setup flow makes: the flow's declarations, no fields. */
export const goIntegrationVersionSetup = (goIntegrationVersionSetupJson satisfies Wire<IntegrationVersion>) as IntegrationVersion;
/** GitHub's page, a setup built-in, before setup has run. */
export const goIntegrationPageSetup = (goIntegrationPageSetupJson satisfies Wire<IntegrationPage>) as IntegrationPage;
/** The same page while its setup flow builds the app: setting up, with the flow under way. */
export const goIntegrationPageSettingUp = (goIntegrationPageSettingUpJson satisfies Wire<IntegrationPage>) as IntegrationPage;
/** The same page once installed, one of its own requirements unmet and its Fix flow waiting on GitHub. */
export const goIntegrationPageFix = (goIntegrationPageFixJson satisfies Wire<IntegrationPage>) as IntegrationPage;
/** The same page after an update whose setup does not take the connection an earlier version made: set it up again. */
export const goIntegrationPageMisfit = (goIntegrationPageMisfitJson satisfies Wire<IntegrationPage>) as IntegrationPage;
/** HubSpot's page switched off: its connection and access kept, no running row. */
export const goIntegrationPageOff = (goIntegrationPageOffJson satisfies Wire<IntegrationPage>) as IntegrationPage;
/** An uploaded bundle's page before any version is approved. */
export const goIntegrationPagePending = (goIntegrationPagePendingJson satisfies Wire<IntegrationPage>) as IntegrationPage;
/** The page of a built-in another version of Kivali shipped. */
export const goIntegrationPageNotIncluded = (goIntegrationPageNotIncludedJson satisfies Wire<IntegrationPage>) as IntegrationPage;
/** The install screen of a browser sign-in whose client Kivali registers itself: nothing to paste. */
export const goIntegrationVersionRegistered = (goIntegrationVersionRegisteredJson satisfies Wire<IntegrationVersion>) as IntegrationVersion;
export const goIntegrationUploadRefused = (goIntegrationUploadRefusedJson satisfies Wire<IntegrationUploadRefused>) as IntegrationUploadRefused;
export const goIntegrationLogs = (goIntegrationLogsJson satisfies Wire<IntegrationLogs>) as IntegrationLogs;
export const goIntegrationAudit = (goIntegrationAuditJson satisfies Wire<IntegrationAudit>) as IntegrationAudit;
/** Request bodies and the options answer, as the Go server's goldens have them. */
export const goConnectionWrite = (goConnectionWriteJson satisfies Wire<ConnectionWrite>) as ConnectionWrite;
export const goAccessWrite = (goAccessWriteJson satisfies Wire<AccessWrite>) as AccessWrite;
export const goAccessChoices = (goAccessChoicesJson satisfies Wire<AccessChoices>) as AccessChoices;
