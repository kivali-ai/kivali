package agent

import (
	"encoding/json"

	"github.com/kivali-ai/kivali/internal/provider"
)

// ShareFileToolName is the stable name of the tool an agent uses to
// drop one or more files into the current chat surface so the user can
// download them inline. The actual file bytes are NOT shipped here —
// the agent references each file by a /files/... path it has access
// to. The chat layer persists a single bubble carrying every
// attachment SHA and the existing /attachments/ download route serves
// the bytes.
const ShareFileToolName = "share_file"

// ShareFileTool is the Anthropic-compatible tool definition. Symmetric
// with publish_*'s `attachments` field for inbox-routed messages — the
// difference is that share_file targets the current conversation
// (direct chat or chat.jsonl) directly, instead of routing through
// the message system. Useful when the user is mid-conversation with
// the agent and wants files there and then.
func ShareFileTool() provider.Tool {
	return provider.Tool{
		Name: ShareFileToolName,
		Description: `Drop one or more files into this conversation as inline attachments the user can download right here, without going through inbox routing. Use this when the user is chatting with you and wants files in the chat — for example, a chart you just produced with run_shell, a PDF from the project corpus, or attachments you received earlier and want to forward back. All files share one chat bubble (and one optional caption); you can mix project files, prior attachments, and your own artifacts in the same call.

Reference each file by a /files/ path you can already see (/files/project/<name>, /files/attachments/<name>, anything you wrote under /files/artifacts/private/, or anything you published under /files/artifacts/public/). Optionally override the filename the user sees on each download link with name.

Access scope: any project file, any attachment in your chat history, your own files under /files/artifacts/private/ and /files/artifacts/public/. You CANNOT share another agent's private attachment.`,
		InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "files": {
      "type": "array",
      "minItems": 1,
      "description": "one or more files to share inline; all land in a single chat bubble",
      "items": {
        "type": "object",
        "properties": {
          "path": {"type": "string", "description": "/files/project/<name>, /files/attachments/<name>, anything under /files/artifacts/private/ or /files/artifacts/public/ (nested paths OK in the artifacts subtrees)"},
          "name": {"type": "string", "description": "optional filename override for the download link; defaults to the path basename (project file's original name, attachment's original name, or your artifact's filename)"}
        },
        "required": ["path"]
      }
    },
    "caption":      {"type": "string", "description": "optional one-line note shown above the file list in the chat bubble"}
  },
  "required": ["files"]
}`),
	}
}
