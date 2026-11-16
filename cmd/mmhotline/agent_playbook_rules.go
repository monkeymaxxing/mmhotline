package main

import (
	"errors"
	"flag"
	"fmt"

	"mmhotline/internal/app"
)

const checkWhetherAPlaybookNeedsAPersonHelp = `usage: mmhotline agent-playbook-rules check-whether-a-playbook-needs-a-person --playbook PATH --inventory NAME [--command-socket PATH] [--json]

Shows whether the triage agent needs a person's approval to run the playbook
on the named inventory, and why. Get permission, then act requires approval.
Warn, wait, then act runs without approval unless someone presses Stop. Act,
then report runs without approval.

MMHotline checks the playbook as MMHotline checks a triage agent request:
with mmx explain in MMHotline's copy of the systems repository. The command
changes nothing. When a review agent is set up, the review agent also reviews
each playbook that would run without a person's approval before the playbook
runs. The check-whether-a-playbook-needs-a-person command does not ask the
review agent.

`

func agentPlaybookRulesFlags(flags *flag.FlagSet, action string, in *app.AgentPlaybookRulesCommandRequest) func() error {
	switch action {
	case "show":
		flags.StringVar(&in.Inventory, "inventory", "", "show only the named inventory, such as production")
	case "check-whether-a-playbook-needs-a-person":
		flags.StringVar(&in.Playbook, "playbook", "", "playbook path relative to the systems repository, such as one_time_playbooks/2026-11-03_restart_postgres_on_db01.yml or playbooks/deploy.yml")
		flags.StringVar(&in.Inventory, "inventory", "", "inventory name: NAME of an inventory file inventories/NAME.yml in the systems repository")
		flags.Usage = func() {
			fmt.Fprint(flags.Output(), checkWhetherAPlaybookNeedsAPersonHelp)
			flags.PrintDefaults()
		}
	}
	return func() error {
		if action != "check-whether-a-playbook-needs-a-person" {
			return nil
		}
		if in.Playbook == "" {
			return errors.New("--playbook is required")
		}
		if in.Inventory == "" {
			return errors.New("--inventory is required")
		}
		return nil
	}
}
