package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"mmhotline/internal/systemsrepository"
	"mmhotline/internal/triageagent"
)

// MMHotline lists the host of each vendor page in the systems repository's
// software_outside_the_distribution.yml at the start of each run there, so
// the software review reads the pages through the logging proxy:
// docs/changing-mmhotline/design/triage-agent.md#the-vendor-pages-hosts. It reads the file in
// its own copy, at the commit the run starts from, never in the agent's
// copy: a commit of the agent's that changes the file waits for a person.

// vendorPagesFile lists the programs on the servers that do not come from
// the distribution, each with its vendor's pages, at the repository's root.
const vendorPagesFile = "software_outside_the_distribution.yml"

// triageAgentVendorPageHostsSchema keeps the latest listing: each page by
// the host and port its address names, its program, and its key.
const triageAgentVendorPageHostsSchema = `
CREATE TABLE triage_agent_vendor_page_hosts (
 host TEXT NOT NULL, port INTEGER NOT NULL, program TEXT NOT NULL,
 page TEXT NOT NULL CHECK(page IN ('releases_page','security_advisories_page')),
 PRIMARY KEY(host, port, program, page)
);
`

// vendorPageAddresses are the two keys MMHotline reads of each program; the
// team's file owns every other key, so no other key can stop the listing.
type vendorPageAddresses struct {
	ReleasesPage           string `yaml:"releases_page"`
	SecurityAdvisoriesPage string `yaml:"security_advisories_page"`
}

// vendorPageWords names each key in a host's reason, which names the keys
// in their sorted order: the releases page first.
var vendorPageWords = map[string]string{"releases_page": "releases page", "security_advisories_page": "security advisories page"}

// vendorPage is one page as a listed host; key is releases_page or
// security_advisories_page.
type vendorPage struct {
	host         string
	port         int
	program, key string
}

// listVendorPageHosts reads the vendor pages in MMHotline's copy at
// commitID, the commit the run starts from, replaces the listed vendor page
// hosts with their hosts, and records with the run why any page was left
// out; the run goes on without it.
func (a *App) listVendorPageHosts(ctx context.Context, runID, commitID string) {
	pages, notListed := readVendorPages(ctx, filepath.Join(a.Store.Dir, systemsrepository.DirectoryName), commitID)
	err := a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM triage_agent_vendor_page_hosts`); err != nil {
			return err
		}
		for _, page := range pages {
			if _, err := tx.ExecContext(ctx, `INSERT INTO triage_agent_vendor_page_hosts(host,port,program,page) VALUES(?,?,?,?)`, page.host, page.port, page.program, page.key); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `UPDATE triage_agent_runs SET vendor_page_hosts_not_listed_because=? WHERE id=?`, strings.Join(notListed, "; "), runID)
		return err
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "mmhotline: could not save the vendor pages' hosts for the triage agent")
	}
}

// readVendorPages reads each program's two pages from the file as commitID
// holds it, and says why it left any out. A missing file lists nothing and
// is no problem; a file MMHotline cannot read lists nothing.
func readVendorPages(ctx context.Context, directory, commitID string) ([]vendorPage, []string) {
	text, found, err := systemsrepository.FileAtCommitIfPresent(ctx, directory, commitID, vendorPagesFile)
	if err == nil && !found {
		return nil, nil
	}
	var file struct {
		Programs map[string]vendorPageAddresses `yaml:"software_outside_the_distribution"`
	}
	if err == nil {
		err = yaml.Unmarshal([]byte(text), &file)
	}
	if err != nil {
		return nil, []string{"MMHotline cannot read " + vendorPagesFile + ": " + err.Error()}
	}
	var pages []vendorPage
	var notListed []string
	for _, program := range slices.Sorted(maps.Keys(file.Programs)) {
		addresses := file.Programs[program]
		for _, page := range [][2]string{{"releases_page", addresses.ReleasesPage}, {"security_advisories_page", addresses.SecurityAdvisoriesPage}} {
			key, address := page[0], page[1]
			if address == "" {
				continue
			}
			host, port, err := vendorPageHost(address)
			if err != nil {
				notListed = append(notListed, fmt.Sprintf("the %s of %s, %s, %s", key, program, address, err))
				continue
			}
			pages = append(pages, vendorPage{host, port, program, key})
		}
	}
	return pages, notListed
}

// vendorPageHost is the host an https:// address names, at port 443 or the
// port the address names; the proxy carries HTTPS only.
func vendorPageHost(address string) (string, int, error) {
	notHTTPS := errors.New("is not an https:// address with a host")
	page, err := url.Parse(address)
	if err != nil || page.Scheme != "https" || page.Hostname() == "" {
		return "", 0, notHTTPS
	}
	port := 443
	if page.Port() != "" {
		if port, err = strconv.Atoi(page.Port()); err != nil {
			return "", 0, notHTTPS
		}
	}
	host, err := triageAgentHost(page.Hostname(), port)
	if err != nil {
		return "", 0, fmt.Errorf("names a host MMHotline cannot list: %s", refusalText(err))
	}
	return host, port, nil
}

// listedVendorPageHosts lists each host of the vendor pages that the latest
// run in the systems repository read, once, with the pages it holds, such
// as "The releases page of caddy and of cloudflared, and the security
// advisories page of caddy, from software_outside_the_distribution.yml".
func listedVendorPageHosts(ctx context.Context, tx *sql.Tx) ([]TriageAgentListedHost, error) {
	rows, err := tx.QueryContext(ctx, `SELECT host,port,page,program FROM triage_agent_vendor_page_hosts ORDER BY host,port,program`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type hostAndPort struct {
		host string
		port int
	}
	var order []hostAndPort
	// programs are each host's programs by the key of their page there.
	programs := map[hostAndPort]map[string][]string{}
	for rows.Next() {
		var at hostAndPort
		var key, program string
		if err := rows.Scan(&at.host, &at.port, &key, &program); err != nil {
			return nil, err
		}
		if programs[at] == nil {
			order = append(order, at)
			programs[at] = map[string][]string{}
		}
		programs[at][key] = append(programs[at][key], program)
	}
	var hosts []TriageAgentListedHost
	for _, at := range order {
		var pages []string
		for _, key := range slices.Sorted(maps.Keys(programs[at])) {
			pages = append(pages, vendorPageWords[key]+" of "+programsText(programs[at][key]))
		}
		hosts = append(hosts, TriageAgentListedHost{ListedHost: triageagent.ListedHost{Host: at.host, Port: at.port, ListedBecause: "The " + strings.Join(pages, ", and the ") + ", from " + vendorPagesFile}})
	}
	return hosts, rows.Err()
}

// programsText joins programs for a page's reason: "caddy", "caddy and of
// cloudflared", or "caddy, of cloudflared, and of mmx".
func programsText(programs []string) string {
	if len(programs) <= 2 {
		return strings.Join(programs, " and of ")
	}
	return strings.Join(programs[:len(programs)-1], ", of ") + ", and of " + programs[len(programs)-1]
}

// vendorPageHostsSettingsNotes is the line settings show prints under
// triage_agent.listed_hosts: the hosts that the latest run listed from the
// vendor pages, which the file need not list.
func vendorPageHostsSettingsNotes(ctx context.Context, tx *sql.Tx) ([]string, error) {
	hosts, err := listedVendorPageHosts(ctx, tx)
	if err != nil || len(hosts) == 0 {
		return nil, err
	}
	var listed []string
	for _, host := range hosts {
		listed = append(listed, host.Host+" port "+strconv.Itoa(host.Port))
	}
	return []string{"Also listed automatically from " + vendorPagesFile + ", as of the latest run: " + strings.Join(listed, ", ")}, nil
}
