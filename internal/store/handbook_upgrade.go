package store

import (
	"errors"
	"fmt"
	"time"

	"github.com/kivali-ai/kivali/internal/graph"
	"github.com/kivali-ai/kivali/internal/handbook"
)

// PreviousHandbookName is the name a handbook that predates the four
// sections is kept under once UpgradeHandbook has set it aside: an
// attachment in the Chief of Staff's chat, read at
// /files/attachments/previous-handbook.md.
const PreviousHandbookName = "previous-handbook.md"

// previousHandbookNote is the runtime's note to the Chief of Staff
// when its team's handbook has been set aside. It is read with
// whatever next wakes the agent and starts no turn of its own.
const previousHandbookNote = "Update from the runtime. Kivali was updated, and the handbook was reorganised with it: the handbook is now the team's, with the sections About, Values, Communicating and Working together, and how the runtime works is Kivali OS, at the top of your system prompt. This team's handbook has been replaced by the default. The handbook it had before is attached as " + PreviousHandbookName + ", under /files/attachments/; nothing from it was carried over. The owner decides what comes back: when they ask, compare the two and propose, with propose_handbook_update, whatever of the previous handbook the team still needs, under the section it belongs to."

// HandbookUpgrade is what UpgradeHandbook did.
type HandbookUpgrade struct {
	// Replaced is true when a handbook without the four sections was
	// set aside and replacement written in its place.
	Replaced bool
	// AttachmentSHA names the attachment the previous text is kept
	// under, content-addressed under attachments/; "" when nothing was
	// replaced.
	AttachmentSHA string
	// ToldChiefOfStaff is true when the note naming the attachment
	// was appended to the Chief of Staff's chat, which is also what
	// makes the file appear under its /files/attachments/.
	ToldChiefOfStaff bool
}

// UpgradeHandbook brings a team's handbook to the four-section shape
// (internal/handbook). A handbook that already has it, or none at all,
// is left alone. One that lacks it, written before the runtime's rules
// moved out of the handbook, is kept whole as an attachment named
// PreviousHandbookName on a quiet note in the Chief of Staff's chat,
// where /files/attachments/ makes it readable, and replacement, which
// must have the shape, is written as the handbook. No part of the old
// text is carried over: the owner has the Chief of Staff bring back
// what the team still needs, through the usual proposal.
//
// Runs at boot and after a restore, like UpgradeModelPins, and is a
// no-op once the handbook has the shape, so a boot that failed midway
// finishes the job on the next. The attachment is stored before the
// note and the note before the handbook, so a failure leaves the old
// text where the next run finds it again rather than gone.
func (s *FSStore) UpgradeHandbook(replacement string, now time.Time) (HandbookUpgrade, error) {
	body, err := s.ReadHandbook()
	if errors.Is(err, ErrNotFound) {
		return HandbookUpgrade{}, nil
	}
	if err != nil {
		return HandbookUpgrade{}, err
	}
	if handbook.Validate(body) == nil {
		return HandbookUpgrade{}, nil
	}
	if err := handbook.Validate(replacement); err != nil {
		return HandbookUpgrade{}, fmt.Errorf("replacement handbook: %w", err)
	}
	att, err := s.AddAttachmentFromText(PreviousHandbookName, body)
	if err != nil {
		return HandbookUpgrade{}, fmt.Errorf("keep the previous handbook: %w", err)
	}
	up := HandbookUpgrade{Replaced: true, AttachmentSHA: att.SHA}
	if _, err := s.GetAgent(graph.ChiefOfStaffSlug); err == nil {
		note := ChatMessage{
			Role:        RoleReceived,
			Kind:        KindWakeUpdate,
			Quiet:       true,
			TS:          now.UTC(),
			Content:     previousHandbookNote,
			Attachments: []MessageAttachment{{SHA: att.SHA, Name: PreviousHandbookName}},
		}
		if err := s.AppendChatMessage(graph.ChiefOfStaffSlug, note); err != nil {
			return up, fmt.Errorf("tell the Chief of Staff: %w", err)
		}
		up.ToldChiefOfStaff = true
	}
	if err := s.WriteHandbook(replacement); err != nil {
		return up, err
	}
	return up, nil
}
