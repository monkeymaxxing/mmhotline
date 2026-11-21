package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"mmhotline/internal/statuspage"
)

// StatusPageCommandRequest holds the inputs of `mmhotline status-page` post,
// resolve, edit, and delete. Every field but the clear flags is a pointer: nil
// means the flag was not given, which is not the same as an empty or zero
// value. A clear flag is true only when the person gave it.
type StatusPageCommandRequest struct {
	NoticeID             *int64    `json:"notice_id,omitempty"`
	UpdateID             *int64    `json:"update_id,omitempty"`
	Title                *string   `json:"title,omitempty"`
	Impact               *string   `json:"impact,omitempty"`
	AffectedComponentIDs *[]string `json:"affected_component_ids,omitempty"`
	Stage                *string   `json:"stage,omitempty"`
	Message              *string   `json:"message,omitempty"`
	ClosingMessage       *string   `json:"closing_message,omitempty"`
	ScheduledStartAt     *string   `json:"scheduled_start_at,omitempty"`
	ScheduledEndAt       *string   `json:"scheduled_end_at,omitempty"`
	ClearComponents      bool      `json:"clear_components,omitempty"`
	ClearSchedule        bool      `json:"clear_schedule,omitempty"`
	ClearStage           bool      `json:"clear_stage,omitempty"`
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// checkedNoticeID returns the notice ID of a request that names a notice.
func (r StatusPageCommandRequest) checkedNoticeID() (int64, error) {
	if r.NoticeID == nil {
		return 0, errors.New("--notice-id is required")
	}
	if *r.NoticeID < 1 {
		return 0, errors.New("--notice-id must be 1 or more")
	}
	return *r.NoticeID, nil
}

// scheduledTime reads a planned time given as RFC 3339, such as
// 2026-11-21T02:00:00Z; nil when the flag was not given.
func scheduledTime(value *string, flagName string) (*time.Time, error) {
	if value == nil {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, *value)
	if err != nil {
		return nil, fmt.Errorf("--%s must be a time such as 2026-11-21T02:00:00Z, not %q", flagName, *value)
	}
	return &parsed, nil
}

// PostChange is the post a request asks for: an update when it names a notice
// with --notice-id, otherwise a new notice. The server and a responder's
// laptop, which posts straight to the R2 bucket, both build it here, so both
// accept and refuse the same requests. An error is the reason a request is
// refused, such as "--title is required to post a new notice".
func (r StatusPageCommandRequest) PostChange() (statuspage.Change, error) {
	if valueOrEmpty(r.Message) == "" {
		return nil, errors.New("--message is required")
	}
	if r.NoticeID != nil {
		noticeID, err := r.checkedNoticeID()
		if err != nil {
			return nil, err
		}
		if r.Title != nil || r.Impact != nil || r.AffectedComponentIDs != nil || r.ScheduledStartAt != nil || r.ScheduledEndAt != nil {
			return nil, errors.New("--title, --impact, --component, --scheduled-start, and --scheduled-end describe a new notice; with --notice-id, post an update with --message and --stage, or change the notice with mmhotline status-page edit")
		}
		return statuspage.PostUpdate(statuspage.PostUpdateInput{NoticeID: noticeID, Stage: statuspage.Stage(valueOrEmpty(r.Stage)), Message: *r.Message}), nil
	}
	if valueOrEmpty(r.Title) == "" {
		return nil, errors.New("--title is required to post a new notice")
	}
	impact := statuspage.ImpactOutage
	if r.Impact != nil {
		impact = statuspage.Impact(*r.Impact)
	}
	var affected []string
	if r.AffectedComponentIDs != nil {
		affected = *r.AffectedComponentIDs
	}
	start, err := scheduledTime(r.ScheduledStartAt, "scheduled-start")
	if err != nil {
		return nil, err
	}
	end, err := scheduledTime(r.ScheduledEndAt, "scheduled-end")
	if err != nil {
		return nil, err
	}
	return statuspage.PostNotice(statuspage.PostNoticeInput{Title: *r.Title, Impact: impact, Stage: statuspage.Stage(valueOrEmpty(r.Stage)), Message: *r.Message, ScheduledStartAt: start, ScheduledEndAt: end, AffectedComponentIDs: affected}), nil
}

// ResolveChange is the resolve a request asks for. Like PostChange, the
// server and a responder's laptop both build it here.
func (r StatusPageCommandRequest) ResolveChange() (statuspage.Change, error) {
	noticeID, err := r.checkedNoticeID()
	if err != nil {
		return nil, err
	}
	return statuspage.Resolve(statuspage.ResolveInput{NoticeID: noticeID, Stage: statuspage.Stage(valueOrEmpty(r.Stage)), ClosingMessage: valueOrEmpty(r.ClosingMessage)}), nil
}

// checkedNoticeAndUpdateIDs returns the notice ID of an edit or delete, and
// the update ID when the request names an update; every ID given must be 1 or
// more, so an explicit --update-id 0 is refused, not taken for no update.
func (r StatusPageCommandRequest) checkedNoticeAndUpdateIDs() (noticeID int64, updateID *int64, err error) {
	if noticeID, err = r.checkedNoticeID(); err != nil {
		return 0, nil, err
	}
	if r.UpdateID != nil {
		if *r.UpdateID < 1 {
			return 0, nil, errors.New("--update-id must be 1 or more")
		}
		updateID = r.UpdateID
	}
	return noticeID, updateID, nil
}

// EditChange is the edit a request asks for: of an update when it names one
// with --update-id, otherwise of the notice. It changes only what the request
// gives. Like PostChange, the server and a responder's laptop both build it
// here.
func (r StatusPageCommandRequest) EditChange() (statuspage.Change, error) {
	noticeID, updateID, err := r.checkedNoticeAndUpdateIDs()
	if err != nil {
		return nil, err
	}
	for _, conflict := range []struct {
		given        bool
		flag, clears string
	}{
		{r.AffectedComponentIDs != nil && r.ClearComponents, "--component", "--clear-components"},
		{r.Stage != nil && r.ClearStage, "--stage", "--clear-stage"},
		{r.ScheduledStartAt != nil && r.ClearSchedule, "--scheduled-start", "--clear-schedule"},
		{r.ScheduledEndAt != nil && r.ClearSchedule, "--scheduled-end", "--clear-schedule"},
	} {
		if conflict.given {
			return nil, fmt.Errorf("%s and %s cannot be given together", conflict.flag, conflict.clears)
		}
	}
	noticeFlagGiven := r.Title != nil || r.Impact != nil || r.AffectedComponentIDs != nil || r.ClearComponents || r.ScheduledStartAt != nil || r.ScheduledEndAt != nil || r.ClearSchedule
	updateFlagGiven := r.Message != nil || r.Stage != nil || r.ClearStage
	if updateID == nil {
		if updateFlagGiven {
			return nil, errors.New("--message, --stage, and --clear-stage change an update; add --update-id to edit one")
		}
		if !noticeFlagGiven {
			return nil, errors.New("edit needs at least one of --title, --impact, --component, --clear-components, --scheduled-start, --scheduled-end, or --clear-schedule")
		}
		in := statuspage.EditNoticeInput{NoticeID: noticeID, Title: r.Title, ClearSchedule: r.ClearSchedule}
		if r.Impact != nil {
			impact := statuspage.Impact(*r.Impact)
			in.Impact = &impact
		}
		switch {
		case r.ClearComponents:
			in.AffectedComponentIDs = &[]string{}
		case r.AffectedComponentIDs != nil:
			in.AffectedComponentIDs = r.AffectedComponentIDs
		}
		if in.ScheduledStartAt, err = scheduledTime(r.ScheduledStartAt, "scheduled-start"); err != nil {
			return nil, err
		}
		if in.ScheduledEndAt, err = scheduledTime(r.ScheduledEndAt, "scheduled-end"); err != nil {
			return nil, err
		}
		in.SetScheduledStartAt, in.SetScheduledEndAt = r.ScheduledStartAt != nil, r.ScheduledEndAt != nil
		return statuspage.EditNotice(in), nil
	}
	if noticeFlagGiven {
		return nil, errors.New("--title, --impact, --component, --clear-components, --scheduled-start, --scheduled-end, and --clear-schedule change the notice; leave out --update-id to edit the notice")
	}
	if !updateFlagGiven {
		return nil, errors.New("edit with --update-id needs at least one of --message, --stage, or --clear-stage")
	}
	if r.Stage != nil && *r.Stage == "" {
		return nil, errors.New("--stage must not be empty; use --clear-stage to remove an update's stage")
	}
	in := statuspage.EditUpdateInput{NoticeID: noticeID, UpdateID: *updateID, Message: r.Message, SetStage: r.Stage != nil || r.ClearStage}
	if r.Stage != nil {
		in.Stage = statuspage.Stage(*r.Stage)
	}
	return statuspage.EditUpdate(in), nil
}

// DeleteChange is the delete a request asks for: of an update when it names
// one with --update-id, otherwise of the whole notice.
func (r StatusPageCommandRequest) DeleteChange() (statuspage.Change, error) {
	noticeID, updateID, err := r.checkedNoticeAndUpdateIDs()
	if err != nil {
		return nil, err
	}
	if updateID == nil {
		return statuspage.DeleteNotice(statuspage.DeleteNoticeInput{NoticeID: noticeID}), nil
	}
	return statuspage.DeleteUpdate(statuspage.DeleteUpdateInput{NoticeID: noticeID, UpdateID: *updateID}), nil
}

// EditOrDeleteDoneMessage says what an edit or delete did, such as "Deleted
// update 2 of notice 1.".
func (r StatusPageCommandRequest) EditOrDeleteDoneMessage(action string, noticeID int64) string {
	verb := map[string]string{"edit": "Edited", "delete": "Deleted"}[action]
	if r.UpdateID != nil {
		return fmt.Sprintf("%s update %d of notice %d.", verb, *r.UpdateID, noticeID)
	}
	return fmt.Sprintf("%s notice %d.", verb, noticeID)
}

func (a *App) statusPageCommand(ctx context.Context, p Principal, action string, request StatusPageCommandRequest, result *CommandResult) error {
	var view StatusPageView
	var noticeID int64
	var err error
	switch action {
	case "show":
		view, err = a.StatusPage(ctx, p)
	case "post", "resolve":
		build, message := request.PostChange, "Posted to notice %d."
		if action == "resolve" {
			build, message = request.ResolveChange, "Notice %d is resolved."
		}
		change, refusal := build()
		if refusal != nil {
			return fmt.Errorf("%w: %s", ErrInvalid, refusal)
		}
		view, noticeID, err = a.publishStatusPage(ctx, p, change)
		result.Message = fmt.Sprintf(message, noticeID)
	case "edit", "delete":
		build := request.EditChange
		if action == "delete" {
			build = request.DeleteChange
		}
		change, refusal := build()
		if refusal != nil {
			return fmt.Errorf("%w: %s", ErrInvalid, refusal)
		}
		view, noticeID, err = a.publishStatusPage(ctx, p, change)
		result.Message = request.EditOrDeleteDoneMessage(action, noticeID)
	default:
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	result.StatusPage, result.StatusPageNoticeID = &view, noticeID
	return nil
}
