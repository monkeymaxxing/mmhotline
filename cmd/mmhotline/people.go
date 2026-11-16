package main

import (
	"errors"
	"flag"

	"mmhotline/internal/app"
)

func peopleFlags(flags *flag.FlagSet, action string, request *app.PeopleCommandRequest) func() error {
	if action == "create-sign-in-link" {
		flags.StringVar(&request.SignInEmailAddress, "sign-in-email-address", "", "person's sign-in email address")
	}
	return func() error {
		if action == "create-sign-in-link" && request.SignInEmailAddress == "" {
			return errors.New("--sign-in-email-address is required")
		}
		return nil
	}
}
