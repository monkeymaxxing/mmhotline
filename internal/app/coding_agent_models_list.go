package app

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"mmhotline/internal/triageagent"
)

// The daily and by-hand sign-in check of an API key asks the vendor's models
// list, which costs nothing and proves the key, instead of running the
// coding agent: docs/changing-mmhotline/design/triage-agent.md#the-daily-sign-in-check.

// codingAgentAPIBaseURLs are the vendors' API addresses the API-key sign-in
// check asks, by coding agent. With the development fake sending services
// (MMHOTLINE_FAKE_SENDING_SERVICES_URL), the check asks their stand-ins
// instead, as every other outside service does; package tests set their own.
func codingAgentAPIBaseURLs(fakeSendingServicesURL string) map[string]string {
	if fakeSendingServicesURL != "" {
		return map[string]string{"claude_code": fakeSendingServicesURL + "/anthropic", "codex": fakeSendingServicesURL + "/openai"}
	}
	return map[string]string{"claude_code": "https://api.anthropic.com", "codex": "https://api.openai.com"}
}

// modelsListCheckTimeLimit ends an API-key sign-in check. A models list
// answers within about a second; 30 seconds allows a slow network and
// still frees the one triage run soon.
const modelsListCheckTimeLimit = 30 * time.Second

// anthropicAPIVersion is the anthropic-version header Anthropic's API
// requires on every request; 2023-06-01 is the version Anthropic's
// documentation gives for the models list.
const anthropicAPIVersion = "2023-06-01"

// checksSignInWithModelsList says whether a run is an API key's sign-in
// check, which asks the models list instead of the coding agent. Other
// credential types sign in through the coding agent, so their check runs
// the setup test's prompt; the setup test itself always runs the prompt,
// because it also tests the model and the reasoning effort.
func checksSignInWithModelsList(purpose, credentialType string) bool {
	return signInCheckPurpose(purpose) && credentialType == "api_key"
}

// checkAPIKeyWithModelsList asks the coding agent's vendor for its models
// list with the request's API key and returns the check's end as a run's
// final event: finished when the vendor answers 200, failed with a plain
// reason otherwise. The reason never shows the key: MMHotline sends the key
// only in a header and reads nothing of the answer but its status.
func (a *App) checkAPIKeyWithModelsList(ctx context.Context, request triageagent.Request) triageagent.Event {
	vendor := map[string]string{"claude_code": "Anthropic", "codex": "OpenAI"}[request.CodingAgent]
	base, known := a.triageAgent.codingAgentAPIBaseURLs[request.CodingAgent]
	if !known {
		return triageagent.Event{Event: "failed", Reason: "MMHotline knows no models list for the coding agent " + request.CodingAgent + "."}
	}
	address, err := url.JoinPath(base, "v1", "models")
	if err != nil {
		return triageagent.Event{Event: "failed", Reason: "MMHotline could not make the address of " + vendor + "'s models list."}
	}
	ctx, cancel := context.WithTimeout(ctx, modelsListCheckTimeLimit)
	defer cancel()
	ask, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return triageagent.Event{Event: "failed", Reason: "MMHotline could not ask " + vendor + "'s models list at " + address + "."}
	}
	if request.CodingAgent == "claude_code" {
		ask.Header.Set("x-api-key", request.Credential)
		ask.Header.Set("anthropic-version", anthropicAPIVersion)
	} else {
		ask.Header.Set("Authorization", "Bearer "+request.Credential)
	}
	answer, err := http.DefaultClient.Do(ask)
	if err != nil {
		return triageagent.Event{Event: "failed", Reason: "MMHotline could not reach " + vendor + "'s models list at " + address + ": " + err.Error()}
	}
	answer.Body.Close()
	if answer.StatusCode == http.StatusOK {
		return triageagent.Event{Event: "finished", Answer: vendor + "'s models list at " + address + " answered 200 OK to the API key, so the key works."}
	}
	why := ""
	switch answer.StatusCode {
	case http.StatusUnauthorized:
		why = ": the API key is wrong, or was deleted or disabled"
	case http.StatusForbidden:
		why = ": the API key may not use the API"
	case http.StatusTooManyRequests:
		why = ": the account is over its rate limit or out of credit"
	}
	return triageagent.Event{Event: "failed", Reason: vendor + "'s models list at " + address + " answered " + answer.Status + why + "."}
}
