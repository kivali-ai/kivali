// Setup and sign-in fixtures: the Go golden files, checked against the generated types with
// `satisfies` so a shape drift fails the TypeScript build.
import loginJson from '../../../../internal/web/apitypes/testdata/login.json';
import setupJson from '../../../../internal/web/apitypes/testdata/setup.json';
import setupProgressFailedJson from '../../../../internal/web/apitypes/testdata/setup_progress_failed.json';
import setupProgressJson from '../../../../internal/web/apitypes/testdata/setup_progress.json';
import type { Login, Setup, SetupProgress } from '../../api/types.gen';

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

export const goLogin = (loginJson satisfies Wire<Login>) as Login;
export const goSetup = (setupJson satisfies Wire<Setup>) as Setup;
export const goProgress = (setupProgressJson satisfies Wire<SetupProgress>) as SetupProgress;
export const goProgressFailed = (setupProgressFailedJson satisfies Wire<SetupProgress>) as SetupProgress;
