package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/compliance-framework/factory/internal/apiclient"
	"github.com/compliance-framework/factory/internal/server"
)

// envServer is the default --server of factory dispatch.
const envServer = "FACTORY_SERVER"

// defaultServer is where factory dispatch posts when neither --server nor FACTORY_SERVER is set.
const defaultServer = "http://127.0.0.1:8080"

// dispatchTimeout bounds the request.
const dispatchTimeout = 30 * time.Second

// maxEventFile bounds the event file (the API's body limit).
const maxEventFile = 1 << 20

func dispatchCommand() command {
	return command{
		name:    "dispatch",
		summary: "inject an event through the API (POST /commands, type dispatch)",
		run: func(ctx context.Context, args []string, stdout, stderr io.Writer) int {
			return runDispatch(ctx, args, os.Getenv, stdout, stderr)
		},
	}
}

func runDispatch(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("factory dispatch", flag.ContinueOnError)
	fl.SetOutput(stderr)
	eventFile := fl.String("event", "", "the event to inject, as JSON (an Event: source, type, subject, data, …)")
	srvURL := fl.String("server", firstNonEmpty(getenv(envServer), defaultServer), "the dispatcher's base URL (env "+envServer+")")
	fl.Usage = func() {
		fmt.Fprintln(stderr, "usage: factory dispatch --event file.json [--server URL]")
		fmt.Fprintln(stderr, "\nPosts {type: dispatch, args: {event}} to <server>/api/v1/commands with the CLI token from "+envCLIToken+".")
		fmt.Fprintln(stderr, "An event without an id gets inject/<request id>. Prints the stored event's id.")
		fl.PrintDefaults()
	}
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitUsage
	}
	if fl.NArg() > 0 {
		fmt.Fprintf(stderr, "factory dispatch: unexpected arguments %v\n", fl.Args())
		return exitUsage
	}
	if *eventFile == "" {
		fmt.Fprintln(stderr, "factory dispatch: --event is required")
		return exitUsage
	}
	token := getenv(envCLIToken)
	if token == "" {
		fmt.Fprintf(stderr, "factory dispatch: %s is not set\n", envCLIToken)
		return exitUsage
	}
	base, err := url.Parse(*srvURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		fmt.Fprintf(stderr, "factory dispatch: --server %q must be an absolute http(s) URL\n", *srvURL)
		return exitUsage
	}
	f, err := os.Open(*eventFile)
	if err != nil {
		fmt.Fprintf(stderr, "factory dispatch: %v\n", err)
		return exitUsage
	}
	data, err := io.ReadAll(io.LimitReader(f, maxEventFile+1))
	_ = f.Close()
	if err == nil && len(data) > maxEventFile {
		err = fmt.Errorf("larger than %d bytes", maxEventFile)
	}
	var ev apiclient.Event
	if err == nil {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		err = dec.Decode(&ev)
	}
	if err != nil {
		fmt.Fprintf(stderr, "factory dispatch: %s: %v\n", *eventFile, err)
		return exitUsage
	}

	c, err := apiclient.NewClientWithResponses(strings.TrimRight(base.String(), "/")+server.BasePath,
		apiclient.WithHTTPClient(&http.Client{Timeout: dispatchTimeout}),
		apiclient.WithRequestEditorFn(func(_ context.Context, r *http.Request) error {
			r.Header.Set("Authorization", "Bearer "+token)
			return nil
		}))
	if err != nil {
		fmt.Fprintf(stderr, "factory dispatch: %v\n", err)
		return exitUsage
	}
	res, err := c.PostCommandWithResponse(ctx, apiclient.Command{
		Type: apiclient.CommandTypeDispatch,
		Args: &apiclient.CommandArgs{Event: &ev},
	})
	if err != nil {
		fmt.Fprintf(stderr, "factory dispatch: %v\n", err)
		return 1
	}
	if res.JSON202 == nil {
		fmt.Fprintf(stderr, "factory dispatch: %s\n", describeProblem(res.HTTPResponse, res.Body))
		return 1
	}
	id := ""
	if res.JSON202.Event != nil {
		id = *res.JSON202.Event
	}
	fmt.Fprintln(stdout, id)
	return 0
}

// describeProblem summarises an error response: its problem+json title, code and detail when
// it has them, else the status.
func describeProblem(resp *http.Response, body []byte) string {
	var p apiclient.Problem
	if err := json.Unmarshal(body, &p); err != nil || p.Title == "" {
		return resp.Status
	}
	out := resp.Status + ": " + p.Title
	if p.Code != nil && *p.Code != "" {
		out += " (" + *p.Code + ")"
	}
	if p.Detail != nil && *p.Detail != "" {
		out += ": " + *p.Detail
	}
	return out
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
