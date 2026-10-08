package files

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Dispatch runs a single file_* tool call against the backend.
// Input is the raw JSON the model emitted on the tool_use; output is
// a plain-text tool_result body the caller injects into the next
// Claude request + surfaces in the UI.
//
// Errors are returned alongside the body when the operation failed
// in a way the model should see — the caller sets is_error=true on
// the tool_result block. "Not found" / "read-only" / "quota" are all
// user-visible errors, not panics.
func Dispatch(b *Backend, toolName string, rawInput json.RawMessage) (string, bool, error) {
	switch toolName {
	case ToolView:
		return handleView(b, rawInput)
	case ToolCreate:
		return handleCreate(b, rawInput)
	case ToolStrReplace:
		return handleStrReplace(b, rawInput)
	case ToolInsert:
		return handleInsert(b, rawInput)
	case ToolDelete:
		return handleDelete(b, rawInput)
	case ToolRename:
		return handleRename(b, rawInput)
	case ToolCopy:
		return handleCopy(b, rawInput)
	}
	return "", true, fmt.Errorf("unknown file tool: %q", toolName)
}

// Run is Dispatch with file_view's image short-circuit in front: a
// file_view whose path names an image comes back as an ImageView (the
// header line as body, the bytes for a vision block) rather than as
// text. Every other call, and a file_view the probe cannot parse, is
// Dispatch's. It is the one entry point the file_* tools use, wherever
// they execute.
func Run(b *Backend, toolName string, rawInput json.RawMessage) (body string, isError bool, img *ImageView, err error) {
	if toolName == ToolView {
		var probe struct {
			Path string `json:"path"`
		}
		if jerr := json.Unmarshal(rawInput, &probe); jerr == nil && probe.Path != "" {
			view, ok, ierr := TryImageView(b, probe.Path)
			if ierr != nil {
				return ierr.Error(), true, nil, nil
			}
			if ok {
				return view.Header, false, view, nil
			}
		}
	}
	body, isError, err = Dispatch(b, toolName, rawInput)
	return body, isError, nil, err
}

func handleView(b *Backend, raw json.RawMessage) (string, bool, error) {
	var in struct {
		Path        string `json:"path"`
		ViewRange   []int  `json:"view_range"`
		Offset      int    `json:"offset"`
		Limit       int    `json:"limit"`
		Grep        string `json:"grep"`
		GrepContext int    `json:"grep_context"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", true, err
	}
	opts := ViewOptions{
		ViewRange:   in.ViewRange,
		Offset:      in.Offset,
		Limit:       in.Limit,
		Grep:        in.Grep,
		GrepContext: in.GrepContext,
	}
	text, err := b.View(in.Path, opts)
	if err != nil {
		return err.Error(), true, nil
	}
	return text, false, nil
}

func handleCreate(b *Backend, raw json.RawMessage) (string, bool, error) {
	var in struct {
		Path     string `json:"path"`
		FileText string `json:"file_text"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", true, err
	}
	if err := b.Create(in.Path, in.FileText); err != nil {
		return err.Error(), true, nil
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(in.FileText), in.Path), false, nil
}

func handleStrReplace(b *Backend, raw json.RawMessage) (string, bool, error) {
	var in struct {
		Path   string `json:"path"`
		OldStr string `json:"old_str"`
		NewStr string `json:"new_str"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", true, err
	}
	if err := b.StrReplace(in.Path, in.OldStr, in.NewStr); err != nil {
		return err.Error(), true, nil
	}
	return "ok", false, nil
}

func handleInsert(b *Backend, raw json.RawMessage) (string, bool, error) {
	var in struct {
		Path       string `json:"path"`
		InsertLine int    `json:"insert_line"`
		InsertText string `json:"insert_text"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", true, err
	}
	if err := b.Insert(in.Path, in.InsertLine, in.InsertText); err != nil {
		return err.Error(), true, nil
	}
	return "ok", false, nil
}

func handleDelete(b *Backend, raw json.RawMessage) (string, bool, error) {
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", true, err
	}
	if err := b.Delete(in.Path); err != nil {
		if errors.Is(err, ErrNotFound) {
			return "not found", true, nil
		}
		return err.Error(), true, nil
	}
	return "deleted " + in.Path, false, nil
}

func handleRename(b *Backend, raw json.RawMessage) (string, bool, error) {
	var in struct {
		OldPath string `json:"old_path"`
		NewPath string `json:"new_path"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", true, err
	}
	if err := b.Rename(in.OldPath, in.NewPath); err != nil {
		return err.Error(), true, nil
	}
	return "renamed", false, nil
}

func handleCopy(b *Backend, raw json.RawMessage) (string, bool, error) {
	var in struct {
		SrcPath  string `json:"src_path"`
		DestPath string `json:"dest_path"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", true, err
	}
	if err := b.Copy(in.SrcPath, in.DestPath); err != nil {
		return err.Error(), true, nil
	}
	return "copied", false, nil
}
