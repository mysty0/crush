// Package rewind implements fork-style conversation and code rewind.
//
// A rewind selects a past user message and produces a new "fork" session
// that contains the conversation up to (and including) that message. The
// origin session is never modified. Depending on the chosen mode, the
// files edited through Crush's tools are also rolled back on disk to the
// state they had at that message.
package rewind

import (
	"context"
	"fmt"
	"os"

	"github.com/charmbracelet/crush/internal/history"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
)

// Mode selects what a rewind restores.
type Mode string

const (
	// ModeConversation forks the conversation only; files are untouched.
	ModeConversation Mode = "conversation"
	// ModeCode restores files on disk only; the conversation is unchanged
	// and no fork is created.
	ModeCode Mode = "code"
	// ModeBoth forks the conversation and restores files on disk.
	ModeBoth Mode = "both"
)

// RewindPoint identifies a user message the conversation can be rewound to.
type RewindPoint struct {
	MessageID    string
	Preview      string
	CreatedAt    int64
	FilesChanged int // Tracked files modified at or after this message.
}

// Result reports the outcome of a rewind.
type Result struct {
	// ForkedSessionID is the new session created for conversation/both
	// modes; empty for code-only rewinds.
	ForkedSessionID string
	// FilesRestored is the number of files written back to disk.
	FilesRestored int
}

// Service performs rewind operations over the message, session, and file
// history stores.
type Service interface {
	// ListRewindPoints returns the session's user messages, newest first,
	// each annotated with how many tracked files changed at or after it.
	ListRewindPoints(ctx context.Context, sessionID string) ([]RewindPoint, error)

	// Rewind rewinds sessionID to messageID using the given mode and
	// returns the result.
	Rewind(ctx context.Context, sessionID, messageID string, mode Mode) (Result, error)
}

type service struct {
	sessions session.Service
	messages message.Service
	history  history.Service
}

// NewService constructs a rewind [Service].
func NewService(sessions session.Service, messages message.Service, history history.Service) Service {
	return &service{
		sessions: sessions,
		messages: messages,
		history:  history,
	}
}

func (s *service) ListRewindPoints(ctx context.Context, sessionID string) ([]RewindPoint, error) {
	all, err := s.messages.List(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("rewind: list messages: %w", err)
	}
	rank := make(map[string]int, len(all))
	for i, m := range all {
		rank[m.ID] = i
	}

	userMessages, err := s.messages.ListUserMessages(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("rewind: list user messages: %w", err)
	}

	files, err := s.history.ListBySession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("rewind: list session files: %w", err)
	}

	points := make([]RewindPoint, 0, len(userMessages))
	for _, m := range userMessages {
		mr, ok := rank[m.ID]
		if !ok {
			continue
		}
		// Count distinct files whose owning message is at or after this
		// point — i.e. files that would change if we rewind here.
		changed := 0
		seen := make(map[string]struct{})
		for _, f := range files {
			fr, ok := rank[f.MessageID]
			if !ok || fr < mr {
				continue
			}
			if _, dup := seen[f.Path]; !dup {
				seen[f.Path] = struct{}{}
				changed++
			}
		}
		points = append(points, RewindPoint{
			MessageID:    m.ID,
			Preview:      preview(m.Content().Text),
			CreatedAt:    m.CreatedAt,
			FilesChanged: changed,
		})
	}
	return points, nil
}

func (s *service) Rewind(ctx context.Context, sessionID, messageID string, mode Mode) (Result, error) {
	target, err := s.messages.Get(ctx, messageID)
	if err != nil {
		return Result{}, fmt.Errorf("rewind: get target message: %w", err)
	}
	if target.SessionID != sessionID {
		return Result{}, fmt.Errorf("rewind: message %s does not belong to session %s", messageID, sessionID)
	}

	var result Result

	if mode == ModeConversation || mode == ModeBoth {
		forkID, err := s.forkConversation(ctx, sessionID, target)
		if err != nil {
			return Result{}, err
		}
		result.ForkedSessionID = forkID
	}

	if mode == ModeCode || mode == ModeBoth {
		restored, err := s.restoreCode(ctx, sessionID, target.ID)
		if err != nil {
			return result, err
		}
		result.FilesRestored = restored
	}

	return result, nil
}

// forkConversation creates a new session and copies every message in the
// origin session up to and including the target into it, preserving order.
// The cut is by position in the ordered message list (not by timestamp),
// so messages sharing a one-second created_at are handled correctly.
// Returns the new session's ID.
func (s *service) forkConversation(ctx context.Context, sessionID string, target message.Message) (string, error) {
	origin, err := s.sessions.Get(ctx, sessionID)
	if err != nil {
		return "", fmt.Errorf("rewind: get origin session: %w", err)
	}

	all, err := s.messages.List(ctx, sessionID)
	if err != nil {
		return "", fmt.Errorf("rewind: list messages: %w", err)
	}

	fork, err := s.sessions.CreateFork(ctx, forkTitle(origin.Title), sessionID, target.ID)
	if err != nil {
		return "", fmt.Errorf("rewind: create fork session: %w", err)
	}

	for _, m := range all {
		clone := m.Clone()
		if _, err := s.messages.Create(ctx, fork.ID, message.CreateMessageParams{
			Role:             clone.Role,
			Parts:            clone.Parts,
			Model:            clone.Model,
			Provider:         clone.Provider,
			IsSummaryMessage: clone.IsSummaryMessage,
		}); err != nil {
			return "", fmt.Errorf("rewind: copy message into fork: %w", err)
		}
		// Stop after copying the target message (inclusive).
		if m.ID == target.ID {
			break
		}
	}

	return fork.ID, nil
}

// restoreCode writes the tracked files back to disk as they were at the
// target message and returns the number of files written. Selection is by
// message order: for each path, the latest version whose owning message is
// at or before the target in the conversation is used.
func (s *service) restoreCode(ctx context.Context, sessionID, targetMessageID string) (int, error) {
	all, err := s.messages.List(ctx, sessionID)
	if err != nil {
		return 0, fmt.Errorf("rewind: list messages: %w", err)
	}
	// Rank each message by its position so we can compare "at or before".
	rank := make(map[string]int, len(all))
	targetRank := -1
	for i, m := range all {
		rank[m.ID] = i
		if m.ID == targetMessageID {
			targetRank = i
		}
	}
	if targetRank < 0 {
		return 0, fmt.Errorf("rewind: target message %s not found in session", targetMessageID)
	}

	files, err := s.history.ListBySession(ctx, sessionID)
	if err != nil {
		return 0, fmt.Errorf("rewind: list session files: %w", err)
	}

	// For each path, pick the highest-version file whose owning message is
	// at or before the target. Versions are returned in ascending order, so
	// a later qualifying version overwrites an earlier one.
	selected := make(map[string]history.File)
	for _, f := range files {
		r, ok := rank[f.MessageID]
		if !ok || r > targetRank {
			continue
		}
		if cur, exists := selected[f.Path]; !exists || f.Version >= cur.Version {
			selected[f.Path] = f
		}
	}

	restored := 0
	for _, f := range selected {
		if err := os.WriteFile(f.Path, []byte(f.Content), 0o644); err != nil {
			return restored, fmt.Errorf("rewind: restore %s: %w", f.Path, err)
		}
		restored++
	}
	return restored, nil
}

func forkTitle(origin string) string {
	if origin == "" {
		return "Rewound session"
	}
	return origin + " (rewind)"
}

// preview trims a message body to a short single-line summary.
func preview(text string) string {
	const maxLen = 80
	out := make([]rune, 0, maxLen)
	for _, r := range text {
		if r == '\n' || r == '\r' || r == '\t' {
			r = ' '
		}
		out = append(out, r)
		if len(out) >= maxLen {
			out = append(out, '…')
			break
		}
	}
	return string(out)
}
