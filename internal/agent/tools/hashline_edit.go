package tools

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/diff"
	"github.com/charmbracelet/crush/internal/filepathext"
	"github.com/charmbracelet/crush/internal/filetracker"
	"github.com/charmbracelet/crush/internal/fsext"
	"github.com/charmbracelet/crush/internal/hashline"
	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/lsp"
	"github.com/charmbracelet/crush/internal/permission"
)

//go:embed hashline_edit.md
var hashlineEditDescription string

// HashlineEditParams is the input to the hashline edit tool: one or more
// "[PATH#TAG]" sections carrying SWAP/DEL/INS operations.
type HashlineEditParams struct {
	Input string `json:"input" description:"One or more [PATH#TAG] sections with SWAP/DEL/INS ops, using line numbers and tags from your latest Read output"`
}

// hlPlan is a preflighted, ready-to-write section.
type hlPlan struct {
	path      string // absolute source file path
	displayed string // section path as written by the model (for headers/diff)
	oldLF     string
	newLF     string
	isCrlf    bool
	additions int
	removals  int

	// File-level directives.
	remove         bool   // REM: delete path
	moveTo         string // MV: absolute destination path ("" when not moving)
	moveToDisplayed string // MV destination as written by the model
}

// HashlineFileEdit is the per-file diff carried in the edit result metadata so
// the UI can render a diff view for each edited file.
type HashlineFileEdit struct {
	FilePath   string `json:"file_path"`
	OldContent string `json:"old_content"`
	NewContent string `json:"new_content"`
	Additions  int    `json:"additions"`
	Removals   int    `json:"removals"`
}

// HashlineEditResponseMetadata is attached to a successful hashline edit
// response; the "Edit" tool renderer reads it to draw one diff per file.
type HashlineEditResponseMetadata struct {
	Files []HashlineFileEdit `json:"files"`
}

// NewHashlineEditTool builds the line-anchored edit tool used when EditMode is
// hashline. It registers under the "Edit" name (replacing the string Edit +
// MultiEdit pair) so the model always calls the same tool. resolver may be nil,
// in which case block ops (SWAP.BLK/DEL.BLK) are rejected and INS.BLK.POST is
// lowered to a plain insert.
func NewHashlineEditTool(
	lspManager *lsp.Manager,
	permissions permission.Service,
	files history.Service,
	filetracker filetracker.Service,
	store *hashline.Store,
	resolver hashline.BlockResolver,
	workingDir string,
) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		EditToolName,
		hashlineEditDescription,
		func(ctx context.Context, params HashlineEditParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if strings.TrimSpace(params.Input) == "" {
				return fantasy.NewTextErrorResponse("input is required"), nil
			}
			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for editing a file")
			}

			sections, warnings, err := hashline.Parse(params.Input)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			if len(sections) == 0 {
				return fantasy.NewTextErrorResponse("no file sections found; each section starts with `[path#TAG]`"), nil
			}

			// Preflight every section before writing anything: a partial batch
			// must never land.
			plans := make([]hlPlan, 0, len(sections))
			for _, sec := range sections {
				plan, warns, resp, ok := preflightHashlineSection(ctx, sessionID, sec, store, resolver, workingDir)
				if !ok {
					return resp, nil
				}
				warnings = append(warnings, warns...)
				plans = append(plans, plan)
			}

			// Request permission for every file first, so a denial aborts
			// before any write.
			for _, plan := range plans {
				action, desc := "write", fmt.Sprintf("Edit file %s", plan.path)
				newForPerm := plan.newLF
				switch {
				case plan.remove:
					action, desc, newForPerm = "delete", fmt.Sprintf("Delete file %s", plan.path), ""
				case plan.moveTo != "":
					desc = fmt.Sprintf("Move file %s to %s", plan.path, plan.moveTo)
				}
				granted, permErr := permissions.Request(ctx, permission.CreatePermissionRequest{
					SessionID:   sessionID,
					Path:        fsext.PathOrPrefix(plan.path, workingDir),
					ToolCallID:  call.ID,
					ToolName:    EditToolName,
					Action:      action,
					Description: desc,
					Params: EditPermissionsParams{
						FilePath:   plan.path,
						OldContent: plan.oldLF,
						NewContent: newForPerm,
					},
				})
				if permErr != nil {
					return fantasy.ToolResponse{}, permErr
				}
				if !granted {
					return NewPermissionDeniedResponse(), nil
				}
			}

			// Commit: write, record history/tracking, notify LSP, mint fresh
			// tags.
			var out strings.Builder
			fileEdits := make([]HashlineFileEdit, 0, len(plans))
			for i, plan := range plans {
				if err := commitHashlinePlan(ctx, plan, sessionID, files, filetracker); err != nil {
					return fantasy.ToolResponse{}, err
				}
				notifyLSPs(ctx, lspManager, plan.path)
				if plan.moveTo != "" {
					notifyLSPs(ctx, lspManager, plan.moveTo)
				}

				if i > 0 {
					out.WriteString("\n")
				}
				switch {
				case plan.remove:
					store.Invalidate(sessionID, plan.path)
					fmt.Fprintf(&out, "Deleted %s\n", plan.displayed)
					fileEdits = append(fileEdits, HashlineFileEdit{
						FilePath: plan.displayed, OldContent: plan.oldLF, NewContent: "", Removals: plan.removals,
					})
				case plan.moveTo != "":
					store.Relocate(sessionID, plan.path, plan.moveTo)
					newTag := store.Record(sessionID, plan.moveTo, plan.newLF, nil)
					fmt.Fprintf(&out, "Moved %s -> %s\n%s\n(+%d -%d)\n", plan.displayed, plan.moveToDisplayed, hashline.FormatHeader(plan.moveToDisplayed, newTag), plan.additions, plan.removals)
					fileEdits = append(fileEdits, HashlineFileEdit{
						FilePath: plan.moveToDisplayed, OldContent: plan.oldLF, NewContent: plan.newLF, Additions: plan.additions, Removals: plan.removals,
					})
				default:
					newTag := store.Record(sessionID, plan.path, plan.newLF, nil)
					fmt.Fprintf(&out, "%s\n(+%d -%d)\n", hashline.FormatHeader(plan.displayed, newTag), plan.additions, plan.removals)
					fileEdits = append(fileEdits, HashlineFileEdit{
						FilePath: plan.displayed, OldContent: plan.oldLF, NewContent: plan.newLF, Additions: plan.additions, Removals: plan.removals,
					})
				}
			}

			if len(warnings) > 0 {
				out.WriteString("\nWarnings:\n")
				for _, w := range warnings {
					fmt.Fprintf(&out, "- %s\n", w)
				}
			}

			text := out.String()
			for _, plan := range plans {
				text += getDiagnostics(plan.path, lspManager)
			}
			return fantasy.WithResponseMetadata(
				fantasy.NewTextResponse(text),
				HashlineEditResponseMetadata{Files: fileEdits},
			), nil
		},
	)
}

// preflightHashlineSection validates one section against the live file and
// computes its post-edit text. On failure it returns an error tool response
// (ok=false) so the caller can abort the whole batch without writing.
func preflightHashlineSection(
	ctx context.Context,
	sessionID string,
	sec hashline.Section,
	store *hashline.Store,
	resolver hashline.BlockResolver,
	workingDir string,
) (hlPlan, []string, fantasy.ToolResponse, bool) {
	filePath := filepathext.SmartJoin(workingDir, sec.Path)
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		absPath = filePath
	}

	info, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return hlPlan{}, nil, fantasy.NewTextErrorResponse(fmt.Sprintf("file not found: %s. To create a new file, use the Write tool.", sec.Path)), false
		}
		return hlPlan{}, nil, fantasy.NewTextErrorResponse(fmt.Sprintf("failed to access file %s: %v", sec.Path, err)), false
	}
	if info.IsDir() {
		return hlPlan{}, nil, fantasy.NewTextErrorResponse(fmt.Sprintf("path is a directory, not a file: %s", sec.Path)), false
	}

	contentBytes, err := os.ReadFile(filePath)
	if err != nil {
		return hlPlan{}, nil, fantasy.NewTextErrorResponse(fmt.Sprintf("failed to read file %s: %v", sec.Path, err)), false
	}
	oldLF, isCrlf := fsext.ToUnixLineEndings(string(contentBytes))

	liveHash := hashline.ComputeFileHash(oldLF)
	if liveHash != sec.Tag {
		snap, recognized := store.ByHash(sessionID, absPath, sec.Tag)
		// Attempt a safe 3-way recovery: apply the edit to the snapshot the tag
		// names, then merge that change onto the drifted live file. Only used
		// for edits (not REM) and only when the merge is conflict-free.
		if recognized && !sec.Remove {
			resolvedForBase, _, rerr := hashline.ResolveBlockEdits(sec.Edits, snap.Text, sec.Path, resolver)
			if rerr == nil {
				if recovered, ok := hashline.Recover(snap.Text, oldLF, resolvedForBase); ok {
					relForDiff := strings.TrimPrefix(filePath, workingDir)
					_, additions, removals := diff.GenerateDiff(oldLF, recovered, relForDiff)
					plan := hlPlan{
						path:      absPath,
						displayed: sec.Path,
						oldLF:     oldLF,
						newLF:     recovered,
						isCrlf:    isCrlf,
						additions: additions,
						removals:  removals,
					}
					if sec.MoveTo != "" {
						dest := filepathext.SmartJoin(workingDir, sec.MoveTo)
						if abs, aerr := filepath.Abs(dest); aerr == nil {
							dest = abs
						}
						plan.moveTo = dest
						plan.moveToDisplayed = sec.MoveTo
					}
					warn := fmt.Sprintf("file %s changed since it was read; your edit was merged onto the current version. Verify the result.", sec.Path)
					return plan, []string{warn}, fantasy.ToolResponse{}, true
				}
			}
		}
		anchors := hashline.AnchorLines(sec.Edits)
		mismatch := hashline.NewMismatchError(sec.Path, sec.Tag, oldLF, liveHash, recognized, anchors)
		return hlPlan{}, nil, fantasy.NewTextErrorResponse(mismatch.Error()), false
	}

	// Seen-lines provenance: reject hunks anchored on lines the matching read
	// never displayed (e.g. beyond a windowed read). Skipped when the snapshot
	// carries no provenance.
	if snap, ok := store.ByHash(sessionID, absPath, sec.Tag); ok && len(snap.SeenLines) > 0 {
		var unseen []int
		for _, ln := range hashline.AnchorLines(sec.Edits) {
			if _, seen := snap.SeenLines[ln]; !seen {
				unseen = append(unseen, ln)
			}
		}
		if len(unseen) > 0 {
			return hlPlan{}, nil, fantasy.NewTextErrorResponse(fmt.Sprintf(
				"edit anchors on line(s) %v of %s that your latest Read did not display. Re-read that range first, then anchor on the lines it shows.",
				unseen, sec.Path)), false
		}
	}

	// REM: delete the file (edits, if any, are ignored).
	if sec.Remove {
		_, _, removals := diff.GenerateDiff(oldLF, "", strings.TrimPrefix(filePath, workingDir))
		return hlPlan{
			path:      absPath,
			displayed: sec.Path,
			oldLF:     oldLF,
			newLF:     "",
			isCrlf:    isCrlf,
			removals:  removals,
			remove:    true,
		}, nil, fantasy.ToolResponse{}, true
	}

	resolved, blockWarns, err := hashline.ResolveBlockEdits(sec.Edits, oldLF, sec.Path, resolver)
	if err != nil {
		return hlPlan{}, nil, fantasy.NewTextErrorResponse(err.Error()), false
	}

	// Apply line edits (skipped for a pure move with no edits).
	newLF := oldLF
	var warns []string
	if len(resolved) > 0 {
		res, aerr := hashline.Apply(oldLF, resolved)
		if aerr != nil && !(sec.MoveTo != "" && errors.Is(aerr, hashline.ErrNoChange)) {
			if errors.Is(aerr, hashline.ErrNoChange) {
				return hlPlan{}, nil, fantasy.NewTextErrorResponse(fmt.Sprintf(
					"edits to %s parsed and applied cleanly but produced no change: the body rows are byte-identical to the file at the targeted lines. Re-read the file before editing again.",
					sec.Path)), false
			}
			return hlPlan{}, nil, fantasy.NewTextErrorResponse(aerr.Error()), false
		}
		newLF = res.Text
		warns = append(warns, res.Warnings...)
	} else if sec.MoveTo == "" {
		return hlPlan{}, nil, fantasy.NewTextErrorResponse(fmt.Sprintf("section for %s has no edits and no REM/MV directive.", sec.Path)), false
	}

	relForDiff := strings.TrimPrefix(filePath, workingDir)
	_, additions, removals := diff.GenerateDiff(oldLF, newLF, relForDiff)

	warns = append(blockWarns, warns...)
	plan := hlPlan{
		path:      absPath,
		displayed: sec.Path,
		oldLF:     oldLF,
		newLF:     newLF,
		isCrlf:    isCrlf,
		additions: additions,
		removals:  removals,
	}
	if sec.MoveTo != "" {
		dest := filepathext.SmartJoin(workingDir, sec.MoveTo)
		if abs, aerr := filepath.Abs(dest); aerr == nil {
			dest = abs
		}
		plan.moveTo = dest
		plan.moveToDisplayed = sec.MoveTo
	}
	return plan, warns, fantasy.ToolResponse{}, true
}

// commitHashlinePlan writes a preflighted plan to disk and records history and
// read tracking.
func commitHashlinePlan(
	ctx context.Context,
	plan hlPlan,
	sessionID string,
	files history.Service,
	filetracker filetracker.Service,
) error {
	messageID := GetMessageFromContext(ctx)

	// REM: delete the file.
	if plan.remove {
		if err := os.Remove(plan.path); err != nil {
			return fmt.Errorf("failed to delete file: %w", err)
		}
		if _, verr := files.CreateVersion(ctx, sessionID, messageID, plan.path, ""); verr != nil {
			slog.Error("Error creating file history version", "error", verr)
		}
		return nil
	}

	writeContent := plan.newLF
	if plan.isCrlf {
		writeContent, _ = fsext.ToWindowsLineEndings(writeContent)
	}

	// MV: write the result at the destination and remove the source.
	target := plan.path
	if plan.moveTo != "" {
		target = plan.moveTo
		if dir := filepath.Dir(target); dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("failed to create destination directory: %w", err)
			}
		}
	}

	if err := os.WriteFile(target, []byte(writeContent), 0o644); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}
	if plan.moveTo != "" && plan.moveTo != plan.path {
		if err := os.Remove(plan.path); err != nil {
			return fmt.Errorf("failed to remove source after move: %w", err)
		}
	}

	file, err := files.GetByPathAndSession(ctx, target, sessionID)
	if err != nil {
		if _, cerr := files.Create(ctx, sessionID, messageID, target, plan.oldLF); cerr != nil {
			return fmt.Errorf("error creating file history: %w", cerr)
		}
	} else if file.Content != plan.oldLF {
		if _, verr := files.CreateVersion(ctx, sessionID, messageID, target, plan.oldLF); verr != nil {
			slog.Error("Error creating file history version", "error", verr)
		}
	}
	if _, verr := files.CreateVersion(ctx, sessionID, messageID, target, writeContent); verr != nil {
		slog.Error("Error creating file history version", "error", verr)
	}

	filetracker.RecordRead(ctx, sessionID, target)
	return nil
}
