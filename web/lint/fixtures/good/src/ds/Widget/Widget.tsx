// Fixture: src/ds is exempt from the raw-element, style and Radix rules, so this stays clean.
import * as Dialog from "@radix-ui/react-dialog";

export function Widget() {
  return (
    <button style={{ color: "var(--ink)" }}>
      <Dialog.Root />
    </button>
  );
}
