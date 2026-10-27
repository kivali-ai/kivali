package googleworkspace

import (
	"cmp"
	"context"
	"net/url"
	"slices"
	"strings"
)

// Calendar's actions, unprefixed.
const (
	actionListCalendars       = "list_calendars"
	actionListEvents          = "list_events"
	actionGetEvent            = "get_event"
	actionFindFreeTime        = "find_free_time"
	actionCreateEvent         = "create_event"
	actionUpdateEvent         = "update_event"
	actionDeleteEvent         = "delete_event"
	actionRespondToInvitation = "respond_to_invitation"
)

// calendarActions is Calendar's actions, read, write, then send. A
// create_event or update_event call that sets or changes who is invited
// is a send, and waits for the owner's approval; the rest of either is
// a write, so each is one action under the write switch.
var calendarActions = []action{
	{
		name:             actionListCalendars,
		ownerDescription: "List the calendars the agent's access includes. Reads only.",
		bucket:           keyCalendarRead,
	},
	{
		name:             actionListEvents,
		ownerDescription: "List the events on a calendar the access includes, a page at a time. Reads only.",
		bucket:           keyCalendarRead,
	},
	{
		name:             actionGetEvent,
		ownerDescription: "Read one event. Reads only.",
		bucket:           keyCalendarRead,
	},
	{
		name:             actionFindFreeTime,
		ownerDescription: "Find when the calendars the access includes are free. Reads only.",
		bucket:           keyCalendarRead,
	},
	{
		name:             actionCreateEvent,
		ownerDescription: "Create an event on a calendar you own. One that invites people waits for your approval instead, and only with May invite and respond.",
		bucket:           keyCalendarWrite,
	},
	{
		name:             actionUpdateEvent,
		ownerDescription: "Change an event on a calendar you own. A change of who is invited waits for your approval instead, and only with May invite and respond.",
		bucket:           keyCalendarWrite,
	},
	{
		name:             actionDeleteEvent,
		ownerDescription: "Delete an event the agent created.",
		bucket:           keyCalendarWrite,
	},
	{
		name:             actionRespondToInvitation,
		ownerDescription: "Ask to answer an invitation: yes, no or maybe. The answer goes only once you approve it.",
		bucket:           keyCalendarSend,
	},
}

// calendarOptions is the options of calendars: every calendar in the
// account's list (users/me/calendarList), by the name the owner gave it
// or its own, the primary calendar first and the rest by name, capitals
// ignored. A calendar's id is its address.
func (g *GoogleWorkspace) calendarOptions(ctx context.Context, token string) ([]string, error) {
	type calendar struct {
		ID              string `json:"id"`
		Summary         string `json:"summary"`
		SummaryOverride string `json:"summaryOverride"`
		Primary         bool   `json:"primary"`
	}
	var all []calendar
	q := url.Values{"maxResults": {"250"}}
	for range maxPages {
		var page struct {
			Items         []calendar `json:"items"`
			NextPageToken string     `json:"nextPageToken"`
		}
		if err := g.get(ctx, token, googleAPI, "/calendar/v3/users/me/calendarList?"+q.Encode(), &page); err != nil {
			return nil, err
		}
		all = append(all, page.Items...)
		if page.NextPageToken == "" {
			break
		}
		q.Set("pageToken", page.NextPageToken)
	}
	name := func(c calendar) string { return cmp.Or(c.SummaryOverride, c.Summary, c.ID) }
	slices.SortStableFunc(all, func(a, b calendar) int {
		if a.Primary != b.Primary {
			if a.Primary {
				return -1
			}
			return 1
		}
		return strings.Compare(strings.ToLower(name(a)), strings.ToLower(name(b)))
	})
	out := make([]string, 0, len(all))
	for _, c := range all {
		if c.ID != "" {
			out = append(out, option(name(c), c.ID))
		}
	}
	return out, nil
}
