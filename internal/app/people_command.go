package app

import (
	"context"
	"fmt"
)

// PeopleCommandRequest is sent only over the peer-checked local command
// socket. The settings file's people part adds, changes, and deactivates
// people; the command lists them and creates sign-in links.
type PeopleCommandRequest struct {
	SignInEmailAddress string `json:"sign_in_email_address,omitempty"`
}
type PeopleCommandResult struct {
	SignInLinkURL        string   `json:"sign_in_link_url,omitempty"`
	SignInLinkValidUntil string   `json:"sign_in_link_valid_until,omitempty"`
	SSHForwardCommand    string   `json:"ssh_forward_command,omitempty"`
	People               []Member `json:"people,omitempty"`
	WebURL               string   `json:"web_url,omitempty"`
	Message              string   `json:"message"`
}

func (a *App) peopleCommand(ctx context.Context, principal Principal, action string, request PeopleCommandRequest, result *CommandResult) error {
	switch action {
	case "create-sign-in-link":
		return a.createSignInLink(ctx, principal, request.SignInEmailAddress, result)
	case "list":
		var err error
		result.People, err = a.TeamMembers(ctx, principal)
		result.Message = fmt.Sprintf("Listed %s. Open MMHotline at %s.", countText(len(result.People), "person", "people"), a.WebURL)
		return err
	}
	return ErrNotFound
}
