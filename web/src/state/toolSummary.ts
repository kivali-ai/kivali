// One-line summaries for tool calls. Pure string helpers: a summary is a present-continuous phrase while the call runs
// ("Reading foo.go") and past tense once it has finished ("Read foo.go").

/** Shortens a long value; a long path keeps its last component. */
export function short(s: string | undefined, max = 50): string {
  if (!s) return '';
  if (s.length <= max) return s;
  const slash = s.lastIndexOf('/');
  if (slash > 0 && s.length - slash < max) return '…' + s.slice(slash);
  return s.slice(0, max - 1) + '…';
}

/**
 * Reads `"<key>": "…` out of JSON that may still be arriving and returns the decoded string so far, or null
 * when the key has not appeared yet.
 */
export function extractJSONStringField(raw: string, key: string): string | null {
  const needle = '"' + key + '"';
  let i = raw.indexOf(needle);
  if (i < 0) return null;
  i += needle.length;
  while (i < raw.length && /\s/.test(raw.charAt(i))) i++;
  if (raw.charAt(i) !== ':') return null;
  i++;
  while (i < raw.length && /\s/.test(raw.charAt(i))) i++;
  if (raw.charAt(i) !== '"') return null;
  i++;
  let out = '';
  while (i < raw.length) {
    const c = raw.charAt(i);
    if (c === '\\') {
      const next = raw.charAt(i + 1);
      if (next === '') break;
      switch (next) {
        case 'n':
          out += '\n';
          break;
        case 't':
          out += '\t';
          break;
        case 'r':
          out += '\r';
          break;
        case 'u': {
          const hex = raw.slice(i + 2, i + 6);
          if (/^[0-9a-fA-F]{4}$/.test(hex)) {
            out += String.fromCharCode(parseInt(hex, 16));
            i += 6;
            continue;
          }
          return out;
        }
        default:
          out += next;
      }
      i += 2;
      continue;
    }
    if (c === '"') break;
    out += c;
    i++;
  }
  return out;
}

/** The input field that holds a write tool's content, which the preview shows instead of the JSON around it. */
export function toolContentKey(name: string): string | null {
  switch (name) {
    case 'file_create':
      return 'file_text';
    case 'file_insert':
      return 'insert_text';
    case 'file_str_replace':
      return 'new_str';
    default:
      return null;
  }
}

/** What a tool's input looks like while it streams: a write tool's content as text, anything else as raw JSON. */
export function previewForTool(name: string, partial: string): string {
  const key = toolContentKey(name);
  if (!key) return partial;
  const extracted = extractJSONStringField(partial, key);
  return extracted ?? partial;
}

function latestHeading(md: string): string {
  const matches = md.match(/^#{1,6}[ \t]+(.+)$/gm);
  const last = matches?.[matches.length - 1];
  return last ? last.replace(/^#{1,6}[ \t]+/, '').trim() : '';
}

const SUMMARY_KEYS = ['path', 'slug', 'title', 'to', 'command', 'old_path', 'new_path', 'query', 'display_name', 'file_path', 'sha', 'id', 'assignee', 'resolution'];

type Fields = Record<string, unknown>;

function parseFields(raw: string | Fields | undefined): Fields {
  if (raw && typeof raw === 'object') return raw;
  if (typeof raw !== 'string' || !raw) return {};
  try {
    const v: unknown = JSON.parse(raw);
    if (v && typeof v === 'object' && !Array.isArray(v)) return v as Fields;
  } catch {
    // Partial JSON while the input streams: pull what fields have arrived.
  }
  const out: Fields = {};
  for (const k of SUMMARY_KEYS) {
    const v = extractJSONStringField(raw, k);
    if (v !== null) out[k] = v;
  }
  return out;
}

function str(v: unknown): string {
  return typeof v === 'string' ? v : typeof v === 'number' ? String(v) : '';
}

/** Strips the MCP server prefix ("mcp__kivali__assignment_create" is "assignment_create"). */
export function bareToolName(name: string): string {
  const i = name.lastIndexOf('__');
  return name.startsWith('mcp__') && i > 4 ? name.slice(i + 2) : name;
}

/** The tool that dispatches background tasks; its calls render as a subagent group, not a tool call. */
export function isSubagentTool(name: string | undefined): boolean {
  return !!name && bareToolName(name) === 'subagent';
}

/** A present-continuous one-liner for what a tool call is doing, from its (possibly partial) input. Empty when nothing useful can be said. */
export function summarizeTool(name: string, input: string | Fields | undefined): string {
  const o = parseFields(input);
  const writeKey = toolContentKey(bareToolName(name));
  let section = '';
  if (writeKey && typeof input === 'string') {
    const content = extractJSONStringField(input, writeKey);
    if (content) section = latestHeading(content);
  }
  const suffix = section ? ' — ' + section : '';
  const path = str(o.path);
  const title = str(o.title);
  const id = str(o.id);
  switch (bareToolName(name)) {
    case 'file_view':
      return path ? 'Reading ' + short(path) : '';
    case 'file_create':
      return path ? 'Writing ' + short(path) + suffix : '';
    case 'file_str_replace':
      return path ? 'Editing ' + short(path) + suffix : '';
    case 'file_insert':
      return path ? 'Inserting into ' + short(path) + suffix : '';
    case 'file_delete':
      return path ? 'Deleting ' + short(path) : '';
    case 'file_rename':
      return o.old_path && o.new_path ? 'Renaming ' + short(str(o.old_path)) + ' → ' + short(str(o.new_path)) : '';
    case 'publish_agent_role':
      return title ? 'Publishing ' + title : o.slug ? 'Publishing ' + str(o.slug) : '';
    case 'publish_notice':
      return title ? 'Sending notice: ' + short(title) : 'Sending notice';
    case 'publish_ceo_approval_request':
      return title ? 'Requesting approval: ' + title : '';
    case 'publish_ceo_notification':
      return title ? 'Notifying you: ' + title : '';
    case 'run_shell': {
      const cmd = str(o.command);
      return cmd ? 'Running shell: ' + short(cmd.split('\n')[0], 50) : '';
    }
    case 'assignment_create':
      if (o.assignee && title) return 'Opening an assignment for ' + str(o.assignee) + ': ' + short(title);
      return title ? 'Opening an assignment: ' + short(title) : 'Opening an assignment';
    case 'assignment_update':
      return id ? 'Updating #' + id : 'Updating an assignment';
    case 'assignment_close':
      return id ? 'Closing #' + id + (o.resolution ? ' (' + str(o.resolution) + ')' : '') : 'Closing an assignment';
    case 'assignment_reopen':
      return id ? 'Reopening #' + id : 'Reopening an assignment';
    case 'assignment_list':
      return 'Listing assignments';
    case 'assignment_view':
      return id ? 'Reading #' + id : 'Reading an assignment';
    case 'get_org_chart':
      return 'Reading the org chart';
    case 'list_project_files':
      return 'Listing project files';
    case 'list_skills':
      return 'Listing skills';
    case 'read_handbook':
      return o.section ? 'Reading the handbook: ' + short(str(o.section)) : 'Reading the handbook';
    case 'propose_handbook_update':
      return 'Proposing a handbook change';
    case 'read_agent_role':
      return o.slug ? 'Reading ' + str(o.slug) + "'s role" : 'Reading a role';
    case 'search_past_chats':
      return o.query ? 'Searching past chats: ' + short(str(o.query)) : 'Searching past chats';
    case 'agent_memory_append':
      return 'Appending to memory';
    case 'agent_memory_str_replace':
      return 'Editing memory';
    case 'agent_memory_habits_view':
      return 'Reading habits';
    case 'agent_memory_habits_append':
      return 'Appending to habits';
    case 'agent_memory_habits_str_replace':
      return 'Editing habits';
    case 'share_file':
      if (o.display_name) return 'Sharing ' + short(str(o.display_name));
      if (o.file_path) return 'Sharing ' + short(str(o.file_path));
      if (o.sha) return 'Sharing ' + short(str(o.sha).slice(0, 8));
      return 'Sharing a file';
    default:
      return '';
  }
}

const PAST: ReadonlyArray<readonly [RegExp, string]> = [
  [/^Reading\b/, 'Read'],
  [/^Writing\b/, 'Wrote'],
  [/^Editing\b/, 'Edited'],
  [/^Inserting\b/, 'Inserted'],
  [/^Deleting\b/, 'Deleted'],
  [/^Renaming\b/, 'Renamed'],
  [/^Publishing\b/, 'Published'],
  [/^Sending\b/, 'Sent'],
  [/^Requesting\b/, 'Requested'],
  [/^Notifying\b/, 'Notified'],
  [/^Running\b/, 'Ran'],
  [/^Listing\b/, 'Listed'],
  [/^Searching\b/, 'Searched'],
  [/^Appending\b/, 'Appended'],
  [/^Sharing\b/, 'Shared'],
  [/^Opening\b/, 'Opened'],
  [/^Updating\b/, 'Updated'],
  [/^Closing\b/, 'Closed'],
  [/^Reopening\b/, 'Reopened'],
];

/** "Reading foo.go" → "Read foo.go". Only the leading verb changes. */
export function pastTense(summary: string): string {
  for (const [re, repl] of PAST) {
    if (re.test(summary)) return summary.replace(re, repl);
  }
  return summary;
}
