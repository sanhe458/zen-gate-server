package server

import (
	"encoding/json"
	"net/http"
)

// sseWriter is a minimal server-sent-events helper.
type sseWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func newSSE(w http.ResponseWriter) (*sseWriter, error) {
	h := w.Header()
	h.Set("content-type", "text/event-stream")
	h.Set("cache-control", "no-cache")
	h.Set("connection", "keep-alive")
	h.Set("x-accel-buffering", "no")
	f, ok := w.(http.Flusher)
	if !ok {
		return nil, http.ErrNotSupported
	}
	return &sseWriter{w: w, f: f}, nil
}

// event writes one JSON data frame.
func (s *sseWriter) event(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = s.w.Write(append(append([]byte("data: "), data...), '\n', '\n'))
	s.f.Flush()
}

// done writes the OpenAI-style terminator.
func (s *sseWriter) done() {
	_, _ = s.w.Write([]byte("data: [DONE]\n\n"))
	s.f.Flush()
}
