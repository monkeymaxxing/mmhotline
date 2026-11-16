package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"time"
)

// The dashboard's picture of the system reads the monitoring tool's fleet
// view: docs/changing-mmhotline/design/interface.md#the-monitoring-tools-fleet-view.

// fleetViewTimeLimit bounds one read of the fleet view, the command's run
// included. The fleet view answers from the hub's memory in about a second
// even for a large fleet; a hung command must not hold the dashboard longer.
// The service unit's RuntimeMaxSec=20 then ends the command itself.
const fleetViewTimeLimit = 10 * time.Second

// fleetViewSizeLimit bounds the answer MMHotline reads into memory to
// decode. A fleet view takes a few KiB for each server, so 64 MiB fits a
// fleet of more than ten thousand servers; a wrong command that prints
// without end must not fill the memory of the process that sends every alert.
const fleetViewSizeLimit = 64 << 20

// fleetView is the part of the fleet view MMHotline reads; it ignores every
// other field.
type fleetView struct {
	Nodes []fleetViewNode `json:"nodes"`
}

type fleetViewNode struct {
	Name     string `json:"name"`
	State    string `json:"state"`
	ExitCode int    `json:"exit_code"`
	// NodeGroups are the node groups the server is in; a server can be in
	// several, or in none.
	NodeGroups           []string  `json:"this_node_is_in_node_groups"`
	LastReportReceivedAt time.Time `json:"last_report_received_at"`
	Report               *struct {
		Summary struct {
			Attention []fleetViewAttention `json:"attention"`
		} `json:"summary"`
	} `json:"report"`
}

// fleetViewAttention is one check on a server that needs attention.
type fleetViewAttention struct {
	Check   string    `json:"check"`
	Finding string    `json:"finding"`
	State   string    `json:"state"`
	Since   time.Time `json:"since"`
	Message string    `json:"message"`
}

// fleetServer is one server of the fleet view as the dashboard shows it.
type fleetServer struct {
	Name string
	// Groups names its node groups in plain words, such as "App servers,
	// Frankfurt"; "" for a server in none.
	Groups string
	// A server that needs attention, whose exit code is not 0, shows why:
	// its silence, with when its last report arrived, the checks of its last
	// report that need attention, or, with neither, its exit code.
	Silent               bool
	LastReportReceivedAt time.Time
	Checks               []fleetViewAttention
	ExitCode             int
}

// readFleetView connects to the fleet view socket, where systemd runs the
// command as root for each connection, and reads its output until the
// command ends.
func readFleetView(ctx context.Context, socket string) (fleetView, error) {
	ctx, cancel := context.WithTimeout(ctx, fleetViewTimeLimit)
	defer cancel()
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return fleetView{}, fmt.Errorf("MMHotline could not connect to the fleet view socket %s, which sudo mmhotline install sets up: %w", socket, err)
	}
	defer connection.Close()
	deadline, _ := ctx.Deadline()
	_ = connection.SetDeadline(deadline)
	output, err := io.ReadAll(io.LimitReader(connection, fleetViewSizeLimit+1))
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded):
		return fleetView{}, fmt.Errorf("the fleet view command did not finish within %s", durationText(fleetViewTimeLimit))
	case err != nil:
		return fleetView{}, fmt.Errorf("MMHotline could not read the fleet view: %w", err)
	}
	return decodeFleetView(output)
}

// decodeFleetView reads what the fleet view command printed.
func decodeFleetView(output []byte) (fleetView, error) {
	switch {
	case len(output) > fleetViewSizeLimit:
		return fleetView{}, errors.New("the fleet view is larger than MMHotline's 64 MiB limit for the fleet view")
	case len(strings.TrimSpace(string(output))) == 0:
		return fleetView{}, errors.New("the fleet view command printed nothing; check the command's errors with sudo journalctl -u 'mmhotline-fleet-view@*'")
	}
	var view fleetView
	if err := json.Unmarshal(output, &view); err != nil {
		return fleetView{}, fmt.Errorf("the fleet view command printed invalid JSON (%v); check the command's errors with sudo journalctl -u 'mmhotline-fleet-view@*'", err)
	}
	if view.Nodes == nil {
		return fleetView{}, errors.New(`the fleet view command printed JSON without a "nodes" list`)
	}
	return view, nil
}

// fleetServers lists the fleet view's servers that need attention and the
// healthy ones, each sorted by name. A server is listed once, whatever
// groups it is in, so no failing server is left out of the picture.
func fleetServers(view fleetView) (needAttention, healthy []fleetServer) {
	for _, node := range view.Nodes {
		server := fleetServer{Name: node.Name, Groups: plainGroupNames(node.NodeGroups), ExitCode: node.ExitCode}
		if node.ExitCode == 0 {
			healthy = append(healthy, server)
			continue
		}
		server.Silent, server.LastReportReceivedAt = node.State == "silent", node.LastReportReceivedAt
		if node.Report != nil {
			server.Checks = node.Report.Summary.Attention
		}
		needAttention = append(needAttention, server)
	}
	byName := func(a, b fleetServer) int { return strings.Compare(a.Name, b.Name) }
	slices.SortStableFunc(needAttention, byName)
	slices.SortStableFunc(healthy, byName)
	return needAttention, healthy
}

// plainGroupNames writes node groups' names in plain words, as a person
// says them: app_servers is "App servers".
func plainGroupNames(groups []string) string {
	names := make([]string, 0, len(groups))
	for _, group := range groups {
		names = append(names, capitalize(strings.ReplaceAll(group, "_", " ")))
	}
	return strings.Join(names, ", ")
}
