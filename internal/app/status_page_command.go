package app

import (
	"context"
	"fmt"
)

// StatusPageCommandRequest holds the inputs of `mmhotline status-page` post
// and resolve.
type StatusPageCommandRequest struct {
	NoticeID       int64  `json:"notice_id,omitempty"`
	Message        string `json:"message,omitempty"`
	ClosingMessage string `json:"closing_message,omitempty"`
}

func (a *App) statusPageCommand(ctx context.Context, p Principal, action string, request StatusPageCommandRequest, result *CommandResult) error {
	var view StatusPageView
	var noticeID int64
	var err error
	switch action {
	case "show":
		view, err = a.StatusPage(ctx, p)
	case "post":
		view, noticeID, err = a.PostToStatusPage(ctx, p, request.NoticeID, request.Message)
		result.Message = fmt.Sprintf("Posted to notice %d.", noticeID)
	case "resolve":
		view, noticeID, err = a.ResolveStatusPageNotice(ctx, p, request.NoticeID, request.ClosingMessage)
		result.Message = fmt.Sprintf("Notice %d is resolved.", noticeID)
	default:
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	result.StatusPage, result.StatusPageNoticeID = &view, noticeID
	return nil
}
