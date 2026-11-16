package fakesendingservices

import "net/http"

// Stand-ins for Anthropic's and OpenAI's models lists, which MMHotline asks
// to check a coding agent's API key. In development every key works, so a
// development MMHotline never sends a key to a vendor.
func (s *Server) registerModelsListRoutes() {
	answer := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[]}`))
	}
	s.mux.HandleFunc("GET /anthropic/v1/models", answer)
	s.mux.HandleFunc("GET /openai/v1/models", answer)
}
