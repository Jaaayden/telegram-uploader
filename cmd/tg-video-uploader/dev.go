package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"

	"github.com/Jaaayden/telegram-uploader/internal/service"
	"github.com/Jaaayden/telegram-uploader/internal/webui"
)

// serveDev exposes the interface over HTTP for development in a normal
// browser. It is unauthenticated, so it only ever listens on loopback and
// rejects requests addressed to any other host name.
func serveDev(address string, app *service.Service) int {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fatal("--dev-http 地址无效", err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fatal("--dev-http", fmt.Errorf("只允许回环地址，例如 127.0.0.1:4599"))
	}

	var mu sync.Mutex
	subscribers := map[chan []byte]struct{}{}
	app.SetEmitter(func(event string, payload any) {
		encoded, err := json.Marshal(map[string]any{"event": event, "payload": payload})
		if err != nil {
			return
		}
		mu.Lock()
		for subscriber := range subscribers {
			select {
			case subscriber <- encoded:
			default: // a stalled tab must not block the service
			}
		}
		mu.Unlock()
	})

	local := func(handler http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			requestHost, _, err := net.SplitHostPort(r.Host)
			if err != nil {
				requestHost = r.Host
			}
			if ip := net.ParseIP(requestHost); requestHost != "localhost" && (ip == nil || !ip.IsLoopback()) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			handler(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", local(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, webui.Page())
	}))
	mux.HandleFunc("POST /rpc", local(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&request); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		response := map[string]any{"id": request.ID}
		if result, err := app.Handle(request.Method, request.Params); err != nil {
			response["error"] = err.Error()
		} else {
			response["result"] = result
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	mux.HandleFunc("GET /events", local(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		subscriber := make(chan []byte, 256)
		mu.Lock()
		subscribers[subscriber] = struct{}{}
		mu.Unlock()
		defer func() {
			mu.Lock()
			delete(subscribers, subscriber)
			mu.Unlock()
		}()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		flusher.Flush()
		for {
			select {
			case message := <-subscriber:
				_, _ = fmt.Fprintf(w, "data: %s\n\n", message)
				flusher.Flush()
			case <-r.Context().Done():
				return
			}
		}
	}))

	app.Start()
	fmt.Fprintf(os.Stderr, "development interface: http://%s/\n", address)
	if err := http.ListenAndServe(address, mux); err != nil {
		app.Shutdown(true)
		return fatal("--dev-http", err)
	}
	return 0
}
