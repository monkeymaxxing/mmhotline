package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"mmhotline/internal/systemsrepository"
	"mmhotline/internal/triageagent"
)

// MMHotline lists each server of the inventories in its copy of the systems
// repository at port 22 at the start of each run there, so the agent reads
// the servers over SSH through the logging proxy:
// docs/changing-mmhotline/design/triage-agent.md#reading-the-servers-over-ssh. MMHotline's copy
// changes only through commits a person made or approved and the agent's
// commits within its two folders, which cannot change an inventory.

// inventoryServerSSHPort is the one port listed for an inventory server: the
// agent reads servers over SSH, so MMX's own HTTPS port and every other port
// stay unlisted.
const inventoryServerSSHPort = 22

// listInventoryServers runs mmx nodes for each inventory in MMHotline's copy,
// replaces the listed inventory servers with the servers it printed, and
// records with the run why any could not be listed; the run goes on without
// them. It returns the servers for the agent's SSH settings.
func (a *App) listInventoryServers(ctx context.Context, runID string) []triageagent.InventoryServer {
	directory := filepath.Join(a.Store.Dir, systemsrepository.DirectoryName)
	type inventoryListing struct {
		inventory string
		servers   []triageagent.InventoryServer
	}
	var listed []inventoryListing
	var notListed []string
	// Under the copy's lock, at the trusted checkout, as for mmx explain.
	err := systemsrepository.ReadWorkingTree(ctx, directory, func(string) error {
		inventories := a.inventoryNames()
		if len(inventories) == 0 {
			return nil
		}
		program, err := FindMMXProgram(a.triageAgent.mmxSearchPath)
		if err != nil {
			return err
		}
		for _, inventory := range inventories {
			servers, err := inventoryServers(ctx, program, directory, mmxEnvironment(a.Store.Dir), inventory)
			if err != nil {
				notListed = append(notListed, "inventory "+inventory+": "+err.Error())
			}
			listed = append(listed, inventoryListing{inventory, servers})
		}
		return nil
	})
	if err != nil {
		notListed = append(notListed, err.Error())
	}
	// A server in several inventories, such as util01, gets one SSH host block.
	var servers []triageagent.InventoryServer
	err = a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		servers = nil
		if _, err := tx.ExecContext(ctx, `DELETE FROM triage_agent_inventory_servers`); err != nil {
			return err
		}
		for _, listing := range listed {
			for _, server := range listing.servers {
				if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO triage_agent_inventory_servers(server_name,inventory) VALUES(?,?)`, server.ServerName, listing.inventory); err != nil {
					return err
				}
				if !slices.ContainsFunc(servers, func(listed triageagent.InventoryServer) bool { return listed.ServerName == server.ServerName }) {
					servers = append(servers, server)
				}
			}
		}
		_, err := tx.ExecContext(ctx, `UPDATE triage_agent_runs SET inventory_servers_not_listed_because=? WHERE id=?`, strings.Join(notListed, "; "), runID)
		return err
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "mmhotline: could not save the inventory servers for the triage agent")
	}
	return servers
}

// inventoryServers reads one inventory's servers with mmx nodes. A server
// whose name is not a DNS name is left out with the reason: the proxy and SSH
// reach a server only by its name.
func inventoryServers(ctx context.Context, program, directory string, environment []string, inventory string) ([]triageagent.InventoryServer, error) {
	output, err := readWithMMX(ctx, program, directory, environment, "nodes", "--inventory", inventory, "--json")
	if err != nil {
		return nil, err
	}
	var listing struct {
		SchemaVersion int `json:"schema_version"`
		Nodes         []struct {
			Name    string `json:"name"`
			Address string `json:"address"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(output, &listing); err != nil {
		return nil, fmt.Errorf("mmx nodes printed output MMHotline cannot read: %w", err)
	}
	if listing.SchemaVersion != 1 {
		return nil, fmt.Errorf("mmx nodes printed schema_version %d; MMHotline reads version 1", listing.SchemaVersion)
	}
	var servers []triageagent.InventoryServer
	var refused []string
	for _, node := range listing.Nodes {
		name, err := triageAgentHost(node.Name, inventoryServerSSHPort)
		if err != nil {
			refused = append(refused, fmt.Sprintf("%q", node.Name))
			continue
		}
		servers = append(servers, triageagent.InventoryServer{ServerName: name, HostKeyAlias: node.Address})
	}
	if len(refused) > 0 {
		return servers, fmt.Errorf("left out the servers whose names are not DNS names: %s", strings.Join(refused, ", "))
	}
	return servers, nil
}
