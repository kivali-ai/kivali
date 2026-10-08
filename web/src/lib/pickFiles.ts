/**
 * Opens the browser's file picker from a button (Attach) and resolves with what the person chose; an empty
 * list when they cancel. The design system's FileDrop is a drop area; a plain button needs its own picker.
 */
export function pickFiles(opts: { multiple?: boolean; accept?: string } = {}): Promise<File[]> {
  return new Promise((resolve) => {
    const input = document.createElement('input');
    input.type = 'file';
    input.multiple = opts.multiple ?? true;
    if (opts.accept) input.accept = opts.accept;
    input.addEventListener('change', () => resolve(Array.from(input.files ?? [])), { once: true });
    input.addEventListener('cancel', () => resolve([]), { once: true });
    input.click();
  });
}
