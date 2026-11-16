package app

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mmhotline/internal/triageagent"
)

func triageAgentForbiddenAddress(address netip.Addr) string {
	address = address.Unmap()
	switch {
	case !address.IsValid() || address.Zone() != "":
		return "The address is not a valid IP address."
	case address.IsLoopback():
		return "Loopback addresses are never allowed."
	case address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast():
		return "Link-local addresses, including the cloud metadata service, are never allowed."
	case address.IsUnspecified():
		return "Unspecified addresses are never allowed."
	case address.IsMulticast():
		return "Multicast addresses are never allowed."
	}
	return ""
}

// ServeTriageAgentProxy accepts the socket mounted into each run's network namespace.
// Closing the listener stops admission; CloseTriageAgent closes the active tunnels.
func (a *App) ServeTriageAgentProxy(listener net.Listener) error {
	for {
		connection, err := listener.Accept()
		if err != nil {
			return err
		}
		a.serveTriageAgentProxyConnection(connection)
	}
}

// maximumOpenTunnels bounds each run's open tunnels, and the connections that
// have not named a run they may use, because a misled agent must not use up
// MMHotline's memory and file descriptors.
const maximumOpenTunnels = 64

func (a *App) serveTriageAgentProxyConnection(connection net.Conn) {
	// Counted before reading: a connection that sends nothing holds memory
	// until its read deadline.
	a.triageAgent.mu.Lock()
	counted := a.triageAgent.proxyConnectionsWithoutARun < maximumOpenTunnels
	if counted {
		a.triageAgent.proxyConnectionsWithoutARun++
	}
	a.triageAgent.mu.Unlock()
	if !counted {
		// Refused at once and without reading, so waiting connections
		// cannot pile up.
		a.handleTriageAgentProxyConnection(connection, false)
		return
	}
	go a.handleTriageAgentProxyConnection(connection, true)
}

// proxyRun is the active run whose proxy token the connection named, or nil.
// A run that is ending takes no new connection: a delayed request must never
// outlive its run.
func (runtime *triageAgentRuntime) proxyRun(token string) *triageAgentActiveRun {
	for _, run := range []*triageAgentActiveRun{runtime.activeRun, runtime.activeReview} {
		if run != nil && !run.ending && token != "" && run.request.ProxyToken == token {
			return run
		}
	}
	return nil
}

// handleTriageAgentProxyConnection reads the connection's first line, the
// proxy token of the run it belongs to, which the run's bridge sends: a
// review run and the triage run that waits for it share the proxy socket.
func (a *App) handleTriageAgentProxyConnection(connection net.Conn, counted bool) {
	defer connection.Close()
	var run *triageAgentActiveRun
	registered := false
	defer func() {
		a.triageAgent.mu.Lock()
		if registered {
			delete(run.connections, connection)
		} else if counted {
			a.triageAgent.proxyConnectionsWithoutARun--
		}
		a.triageAgent.mu.Unlock()
		if registered {
			run.tunnels.Done()
		}
	}()
	connection.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReaderSize(connection, 8192)
	reason := ""
	status := 403
	if !counted {
		status = 503
		reason = fmt.Sprintf("%d connections to MMHotline's logging proxy have not yet named a run; another connection would use too much memory and too many file descriptors.", maximumOpenTunnels)
	} else {
		token, err := reader.ReadSlice('\n')
		a.triageAgent.mu.Lock()
		if err == nil {
			run = a.triageAgent.proxyRun(strings.TrimSuffix(string(token), "\n"))
		}
		if run != nil && len(run.connections) >= maximumOpenTunnels {
			status = 503
			reason = fmt.Sprintf("The run already has %d open tunnels; another connection would use too much memory and too many file descriptors.", maximumOpenTunnels)
		} else if run != nil {
			run.connections[connection] = true
			run.tunnels.Add(1)
			registered = true
			a.triageAgent.proxyConnectionsWithoutARun--
		}
		a.triageAgent.mu.Unlock()
	}
	host := ""
	port := 0
	// A connection refused at once sends nothing more MMHotline reads.
	if counted && (reason == "" || status == 503) {
		line, readErr := reader.ReadSlice('\n')
		total := len(line)
		fields := strings.Fields(string(line))
		method := ""
		if len(fields) > 0 {
			method = fields[0]
		}
		if len(fields) > 1 {
			hostname, portText, err := net.SplitHostPort(fields[1])
			if err == nil {
				host = hostname
				port, _ = strconv.Atoi(portText)
			}
		}
		if status == 503 {
			// The request line identifies the refused host; headers are not needed.
		} else if method != "CONNECT" {
			status = 405
			reason = "MMHotline's triage agent proxy accepts only HTTPS through CONNECT."
		} else if readErr != nil || len(fields) != 3 || (fields[2] != "HTTP/1.1" && fields[2] != "HTTP/1.0") {
			reason = "The CONNECT request is invalid."
		}
		for reason == "" {
			header, err := reader.ReadSlice('\n')
			total += len(header)
			if err != nil || total > 8192 {
				reason = "The CONNECT request and headers must be at most 8 KiB."
				break
			}
			if string(header) == "\r\n" || string(header) == "\n" {
				break
			}
			if !strings.Contains(string(header), ":") {
				reason = "The CONNECT headers are invalid."
				break
			}
		}
	}
	connection.SetReadDeadline(time.Time{})
	if reason == "" {
		if run == nil {
			reason = "The connection names no active triage agent or review agent run."
		} else if run.ctx.Err() != nil {
			reason = "The triage agent run has ended."
		}
	}
	var upstream net.Conn
	address := ""
	if reason == "" {
		var err error
		host, err = triageAgentHost(host, port)
		if err != nil {
			reason = publicError(err)
		}
	}
	if reason == "" {
		listed := false
		internetAccess := ""
		reviewAgent := reviewAgentPurpose(run.purpose) //nolint:nilaway // An empty reason means the checks above found a run.
		err := a.Store.WithTransaction(run.ctx, func(tx *sql.Tx) error {
			// A review needs only its prompt and its model: it reaches its
			// coding agent's service and nothing else, whatever the triage
			// agent's internet access.
			if reviewAgent {
				for _, entry := range triageagent.ServiceHosts(run.request.CodingAgent, run.request.CredentialType) {
					listed = listed || entry.Host == host && entry.Port == port
				}
				internetAccess = "listed_hosts_only"
				return nil
			}
			hosts, err := triageAgentHosts(run.ctx, tx, run.request.CodingAgent, run.request.CredentialType, inSystemsRepository(run.purpose))
			if err != nil {
				return err
			}
			for _, entry := range hosts {
				listed = listed || entry.Host == host && entry.Port == port
			}
			return tx.QueryRowContext(run.ctx, `SELECT internet_access FROM triage_agent_internet_access WHERE id=1`).Scan(&internetAccess)
		})
		if err != nil {
			reason = "The internet access settings could not be read."
		} else if !listed && reviewAgent {
			reason = fmt.Sprintf("%s is not the review agent's coding agent service, the only host a review reaches.", net.JoinHostPort(host, strconv.Itoa(port)))
		} else if !listed && internetAccess != "any_site" {
			reason = fmt.Sprintf("%s is not a listed host; an administrator can add it to triage_agent.listed_hosts in MMHotline's settings file.", net.JoinHostPort(host, strconv.Itoa(port)))
		} else {
			addresses := []netip.Addr{}
			if ip, err := netip.ParseAddr(host); err == nil {
				addresses = append(addresses, ip)
			} else {
				resolve := a.triageAgent.resolve
				if resolve == nil {
					resolve = net.DefaultResolver.LookupIPAddr
				}
				ctx, cancel := context.WithTimeout(run.ctx, 10*time.Second)
				resolved, err := resolve(ctx, host)
				cancel()
				if err != nil {
					reason = "The host could not be resolved."
				} else {
					for _, entry := range resolved {
						ip, ok := netip.AddrFromSlice(entry.IP)
						if !ok || entry.Zone != "" {
							reason = "The host resolved to an invalid IP address."
							break
						}
						addresses = append(addresses, ip.Unmap())
					}
				}
			}
			if reason == "" && len(addresses) == 0 {
				reason = "The host has no IP addresses."
			}
			// Check every answer: an unlisted host must resolve only to public addresses.
			for _, ip := range addresses {
				if reason != "" {
					break
				}
				address = ip.String()
				reason = triageAgentForbiddenAddress(ip)
				if reason == "" && !listed && !publicInternetAddress(ip) {
					reason = "An unlisted host must resolve only to public internet addresses."
				}
			}
			if reason == "" {
				address = addresses[0].String()
				target := net.JoinHostPort(address, strconv.Itoa(port))
				dial := a.triageAgent.dial
				if dial == nil {
					dialer := &net.Dialer{Timeout: 15 * time.Second, Control: func(_, target string, _ syscall.RawConn) error {
						actual, _, err := net.SplitHostPort(target)
						if err != nil {
							return err
						}
						ip, err := netip.ParseAddr(actual)
						if err != nil {
							return err
						}
						if ip.Unmap() != addresses[0].Unmap() {
							return fmt.Errorf("the connection address changed")
						}
						if reason := triageAgentForbiddenAddress(ip); reason != "" {
							return fmt.Errorf("%s", reason)
						}
						if !listed && !publicInternetAddress(ip) {
							return errAddressNotPublic
						}
						return nil
					}}
					dial = dialer.DialContext
				}
				ctx, cancel := context.WithTimeout(run.ctx, 15*time.Second)
				upstream, err = dial(ctx, "tcp", target)
				cancel()
				if err != nil {
					reason = "The host could not be reached."
				}
			}
		}
	}
	decision := "allowed"
	if reason != "" {
		decision = "refused"
	}
	runID := ""
	if run != nil {
		runID = run.id
		host = run.redact(host)
		reason = run.redact(reason)
	}
	var connectionID int64
	err := a.Store.WithTransaction(context.Background(), func(tx *sql.Tx) error {
		result, err := tx.Exec(`INSERT INTO triage_agent_proxy_connections(run_id,started_at,host,port,address,decision,refusal_reason) VALUES(?,?,?,?,?,?,?)`, runID, a.Now().Unix(), host, port, address, decision, reason)
		if err != nil {
			return err
		}
		connectionID, err = result.LastInsertId()
		return err
	})
	if err != nil {
		if upstream != nil {
			upstream.Close()
		}
		fmt.Fprintln(os.Stderr, "mmhotline: could not record a triage agent proxy connection")
		return
	}
	var sent, received int64
	defer func() {
		if err := a.Store.WithTransaction(context.Background(), func(tx *sql.Tx) error {
			_, err := tx.Exec(`UPDATE triage_agent_proxy_connections SET bytes_sent=?,bytes_received=?,ended_at=? WHERE id=?`, sent, received, a.Now().Unix(), connectionID)
			return err
		}); err != nil {
			fmt.Fprintln(os.Stderr, "mmhotline: could not save triage agent proxy connection totals")
		}
	}()
	connection.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if reason != "" {
		body := "MMHotline refused the connection: " + reason + "\n"
		title := "Forbidden"
		if status == 405 {
			title = "Method Not Allowed"
		}
		if status == 503 {
			title = "Service Unavailable"
		}
		fmt.Fprintf(connection, "HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", status, title, len(body), body)
		return
	}
	defer upstream.Close()
	// Ending the run also interrupts a copy blocked on the remote side.
	stopClosing := context.AfterFunc(run.ctx, func() { upstream.Close(); connection.Close() })
	defer stopClosing()
	if _, err = io.WriteString(connection, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	connection.SetWriteDeadline(time.Time{})
	type copied struct {
		sent  bool
		bytes int64
	}
	done := make(chan copied, 2)
	copyData := func(destination net.Conn, source io.Reader, isSent bool) {
		count, _ := io.Copy(destination, source)
		if half, ok := destination.(interface{ CloseWrite() error }); ok {
			// The copy has ended; both connections close once both copies end.
			_ = half.CloseWrite()
		} else {
			destination.Close()
		}
		done <- copied{isSent, count}
	}
	go copyData(upstream, reader, true)
	go copyData(connection, upstream, false)
	for range 2 {
		result := <-done
		if result.sent {
			sent = result.bytes
		} else {
			received = result.bytes
		}
	}
}
