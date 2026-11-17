package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/kabeer11000/kiln/internal/kiln"
)

// cmdServe exposes the CLI as a local HTTP API. Every endpoint runs the same handler as the
// matching command with --json, so the API and the CLI can never disagree.
func (a *app) cmdServe(args []string) (int, error) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:7777", "listen address")
	if _, err := a.parseFS(fs, args, 0); err != nil {
		return 2, err
	}
	mux := http.NewServeMux()
	cmd := func(h func(*app, []string) (int, error), args func(*http.Request) ([]string, error)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			argv, err := args(r)
			if err != nil {
				writeErr(w, err)
				return
			}
			a.serveCmd(w, r, h, argv)
		}
	}
	ref := func(r *http.Request) ([]string, error) { return []string{r.PathValue("run")}, nil }
	none := func(*http.Request) ([]string, error) { return nil, nil }

	mux.HandleFunc("GET /v1/runs", cmd((*app).cmdList, none))
	mux.HandleFunc("GET /v1/runs/{run}", cmd((*app).cmdStatus, ref))
	mux.HandleFunc("GET /v1/runs/{run}/logs", cmd((*app).cmdLogs, ref))
	mux.HandleFunc("GET /v1/runs/{run}/top", cmd((*app).cmdTop, func(r *http.Request) ([]string, error) {
		argv := []string{r.PathValue("run")}
		if r.URL.Query().Get("follow") != "" {
			argv = append(argv, "-f")
		}
		return argv, nil
	}))
	mux.HandleFunc("GET /v1/runs/{run}/diff/{other}", cmd((*app).cmdDiff, func(r *http.Request) ([]string, error) {
		return []string{r.PathValue("run"), r.PathValue("other")}, nil
	}))
	mux.HandleFunc("POST /v1/runs/{run}/cancel", cmd((*app).cmdCancel, ref))
	mux.HandleFunc("POST /v1/runs/{run}/send", cmd((*app).cmdSend, func(r *http.Request) ([]string, error) {
		var body struct {
			Message string `json:"message"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil || body.Message == "" {
			return nil, &kiln.Error{Code: "usage", Message: "body must be JSON {\"message\": \"...\"}"}
		}
		return []string{r.PathValue("run"), body.Message}, nil
	}))
	mux.HandleFunc("POST /v1/runs/{run}/artifacts", cmd((*app).cmdArtifacts, ref))
	// Starting runs always detaches: the HTTP call returns the id; follow with /top or /wait.
	mux.HandleFunc("POST /v1/runs", cmd((*app).cmdRun, func(r *http.Request) ([]string, error) {
		var body struct {
			File string `json:"file"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			return nil, &kiln.Error{Code: "usage", Message: "body must be JSON {\"file\": \"path/to/kiln.yaml\"} (file optional)"}
		}
		argv := []string{"-d"}
		if body.File != "" {
			argv = append(argv, "-f", body.File)
		}
		return argv, nil
	}))
	mux.HandleFunc("POST /v1/runs/{run}/rerun", cmd((*app).cmdRerun, func(r *http.Request) ([]string, error) {
		return []string{"-d", r.PathValue("run")}, nil
	}))
	mux.HandleFunc("GET /v1/runs/{run}/wait", cmd((*app).cmdWait, ref))

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return 1, err
	}
	fmt.Fprintf(a.out, "kiln API on http://%s/v1/runs (Ctrl-C to stop)\n", ln.Addr())
	return 1, http.Serve(ln, localOnly(mux))
}

// localOnly blocks the two ways a web page in your browser could reach a localhost API:
// DNS rebinding (checked via Host) and cross-site form posts (POST must be JSON, which
// browsers only send cross-origin after a CORS preflight this server never approves).
func localOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			writeErr(w, &kiln.Error{Code: "forbidden", Message: "host " + r.Host + " not allowed"})
			return
		}
		if r.Method == http.MethodPost && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			writeErr(w, &kiln.Error{Code: "usage", Message: "POST needs Content-Type: application/json"})
			return
		}
		h.ServeHTTP(w, r)
	})
}

// serveCmd runs one CLI handler. Streams (top?follow) go straight out, flushed per line;
// everything else is buffered so the HTTP status can reflect the outcome.
func (a *app) serveCmd(w http.ResponseWriter, r *http.Request, h func(*app, []string) (int, error), argv []string) {
	if r.URL.Query().Get("follow") != "" {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fw := flushWriter{w: w, f: w.(http.Flusher)}
		sub := &app{out: fw, json: true, store: a.store, ctx: r.Context()}
		if _, err := h(sub, argv); err != nil {
			sub.fail(err, 1)
		}
		return
	}
	var buf bytes.Buffer
	sub := &app{out: &buf, json: true, store: a.store, ctx: r.Context()}
	if _, err := h(sub, argv); err != nil {
		writeErr(w, err)
		return
	}
	if json.Valid(buf.Bytes()) {
		w.Header().Set("Content-Type", "application/json")
	} else {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8") // logs
	}
	_, _ = w.Write(buf.Bytes())
}

func writeErr(w http.ResponseWriter, err error) {
	var ke *kiln.Error
	if !errors.As(err, &ke) {
		ke = &kiln.Error{Code: "error", Message: err.Error()}
	}
	status := map[string]int{
		"usage": 400, "run_not_found": 404, "no_logs": 404, "no_artifacts": 404, "forbidden": 403,
		"ambiguous_run": 409, "already_finished": 409, "not_supported": 501,
	}[ke.Code]
	if status == 0 {
		status = 500
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": ke})
}

type flushWriter struct {
	w io.Writer
	f http.Flusher
}

func (fw flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	fw.f.Flush()
	return n, err
}
