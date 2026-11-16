package triageagent

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"

	"mmhotline/internal/systemsrepository"
)

const (
	runStoppingMessage         = "The triage agent run is stopping, so this request gets no answer from MMHotline."
	runEndedMessage            = "The triage agent run ended before MMHotline answered this request."
	noSystemsRepositoryMessage = "This run has no systems repository, so it cannot ask for playbook runs."
)

// requestRelay passes the agent's requests from the commands it runs to
// MMHotline over the run socket, one at a time, and hands each answer back
// to the waiting command.
type requestRelay struct {
	ctx        context.Context
	cancel     context.CancelFunc
	writer     *eventWriter
	agentCopy  string // empty in a run without a systems repository
	base       string
	directory  string
	socketPath string
	listener   net.Listener
	// turn holds the one request being handled; the others wait for it.
	turn chan struct{}

	mutex        sync.Mutex
	open         *openRequest // sent to MMHotline and not yet answered
	ending       chan struct{}
	endedBecause string
}

type openRequest struct {
	id     string
	answer chan Answer
}

// startRequestRelay listens for the agent's commands on a Unix socket in the
// run's private /tmp; a path there also fits the Unix socket path limit.
func startRequestRelay(ctx context.Context, writer *eventWriter, agentCopy, base string) (*requestRelay, error) {
	directory, err := os.MkdirTemp("/tmp", "mmhotline-triage-agent-requests-")
	if err != nil {
		return nil, err
	}
	socketPath := filepath.Join(directory, "request.sock")
	listener, err := net.Listen("unix", socketPath)
	if err == nil {
		// The run is one user; only it connects.
		err = os.Chmod(socketPath, 0o600)
	}
	if err != nil {
		if listener != nil {
			_ = listener.Close()
		}
		_ = os.RemoveAll(directory)
		return nil, err
	}
	relayContext, cancel := context.WithCancel(ctx)
	relay := &requestRelay{ctx: relayContext, cancel: cancel, writer: writer, agentCopy: agentCopy, base: base,
		directory: directory, socketPath: socketPath, listener: listener, turn: make(chan struct{}, 1), ending: make(chan struct{})}
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go relay.serve(connection)
		}
	}()
	return relay, nil
}

func (relay *requestRelay) close() {
	relay.cancel()
	_ = relay.listener.Close()
	_ = os.RemoveAll(relay.directory)
}

// end answers every waiting command with reason and refuses later requests,
// so no request goes up after the run's final event and no command waits
// for an answer that cannot come. A review agent's run has no relay.
func (relay *requestRelay) end(reason string) {
	if relay == nil {
		return
	}
	relay.mutex.Lock()
	defer relay.mutex.Unlock()
	select {
	case <-relay.ending:
	default:
		relay.endedBecause = reason
		close(relay.ending)
	}
}

// deliver hands MMHotline's answer to the command waiting for it. An answer
// that arrives after the run ended the request finds nobody and is dropped,
// as does one for a review agent's run, which has no relay.
func (relay *requestRelay) deliver(answer Answer) {
	if relay == nil {
		return
	}
	relay.mutex.Lock()
	defer relay.mutex.Unlock()
	if relay.open != nil && relay.open.id == answer.RequestID {
		relay.open.answer <- answer
		relay.open = nil
	}
}

func (relay *requestRelay) serve(connection net.Conn) {
	defer connection.Close()
	stopClosing := context.AfterFunc(relay.ctx, func() { _ = connection.Close() })
	defer stopClosing()
	_ = json.NewEncoder(connection).Encode(relay.handle(connection))
}

func (relay *requestRelay) handle(connection net.Conn) Answer {
	refused := func(id, message string) Answer { return Answer{RequestID: id, Outcome: Refused, Message: message} }
	line, err := readRunLine(bufio.NewReader(connection), maximumLineSizeWithoutBundle)
	if err != nil {
		return refused("", "The request could not be read: "+err.Error())
	}
	var request AgentRequest
	if err := json.Unmarshal(line, &request); err != nil {
		return refused("", "The request is not a JSON object: "+err.Error())
	}
	if err := CheckAgentRequest(request); err != nil {
		return refused("", "The request was refused: "+err.Error())
	}
	if request.Kind == PlaybookRunRequest && relay.agentCopy == "" {
		return refused("", noSystemsRepositoryMessage)
	}
	select {
	case relay.turn <- struct{}{}:
		defer func() { <-relay.turn }()
	case <-relay.ending:
		return refused("", relay.endReason())
	}
	event := Event{Event: "report", RequestID: newRequestID(), AlertSeverity: request.AlertSeverity, ReportText: request.ReportText}
	if request.Kind == CommitReviewRequest {
		event.Event, event.CommitID, event.CommitReviewDecision = "commit_review", request.CommitID, request.CommitReviewDecision
	}
	if request.Kind == PlaybookRunRequest {
		event = Event{Event: "playbook_run_requested", RequestID: event.RequestID, Playbook: request.Playbook, Inventory: request.Inventory, Diagnosis: request.Diagnosis}
		// None is fine: the playbook may already be on MMHotline's branch.
		if event.AgentCommitsBundle, err = systemsrepository.AgentCommitsBundle(relay.ctx, relay.agentCopy, relay.base); err != nil {
			return refused(event.RequestID, "Your commits could not be sent to MMHotline: "+err.Error())
		}
	}
	answers, message := relay.send(event)
	if answers == nil {
		return refused(event.RequestID, message)
	}
	select {
	case answer := <-answers:
		return answer
	case <-relay.ending:
	}
	relay.mutex.Lock()
	if relay.open != nil && relay.open.id == event.RequestID {
		relay.open = nil
	}
	relay.mutex.Unlock()
	select {
	case answer := <-answers: // it arrived as the run ended
		return answer
	default:
		return refused(event.RequestID, relay.endReason())
	}
}

// send writes the request's event under the relay's lock, so it cannot follow
// the run's final event, and returns where its answer will arrive.
func (relay *requestRelay) send(event Event) (<-chan Answer, string) {
	relay.mutex.Lock()
	defer relay.mutex.Unlock()
	select {
	case <-relay.ending:
		return nil, relay.endedBecause
	default:
	}
	if err := relay.writer.write(event); err != nil {
		return nil, "The request could not be sent to MMHotline: " + err.Error()
	}
	relay.open = &openRequest{id: event.RequestID, answer: make(chan Answer, 1)}
	return relay.open.answer, ""
}

func (relay *requestRelay) endReason() string {
	relay.mutex.Lock()
	defer relay.mutex.Unlock()
	return relay.endedBecause
}

func newRequestID() string {
	random := make([]byte, 8)
	_, _ = rand.Read(random)
	return hex.EncodeToString(random)
}
