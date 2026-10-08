// Work fixtures. The Go golden files are what the server really emits; `satisfies` fails the build when they
// lose a field or change its type, and the cast only narrows the string unions a JSON import widens.
import goAssignmentJson from '../../../../internal/web/apitypes/testdata/assignment.json';
import goWorkJson from '../../../../internal/web/apitypes/testdata/work.json';
import type { Assignment, WorkBoard } from '../../api/types.gen';

/** The generated type with every string union widened to string: what a JSON import can be checked against. */
export type Wire<T> = T extends string
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

export const goWork = (goWorkJson satisfies Wire<WorkBoard>) as WorkBoard;
/** On hold under #40 by you, with an outcome the golden file carries so the Outcome card is exercised. */
export const goAssignment = (goAssignmentJson satisfies Wire<Assignment>) as Assignment;

/** The board with nothing on it. */
export const emptyWork: WorkBoard = {
  readouts: { open: 0, ready: 0, blocked: 0, on_hold: 0, closed_week: 0 },
  goals: [],
  closed_goals: [],
};

const { outcome: _outcome, held_by: _heldBy, ...rest } = goAssignment;
void _outcome;
void _heldBy;

/** An open, ready assignment nobody holds: every Act item but Reopen is live. */
export const openAssignment: Assignment = { ...rest, state: 'ready', why: '', held_here: false };

/** The same assignment closed as done: only Reopen is live. */
export const closedAssignment: Assignment = { ...goAssignment, state: 'closed', why: '' };

/** On hold by its own hold: Release hold replaces Put on hold. */
export const heldHereAssignment: Assignment = { ...goAssignment, held_here: true };
