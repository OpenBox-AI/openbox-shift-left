// Command refusal-injector is probe A's instrument. Never against real work: a
// refusal injected mid-conversation can leave the session's context in a state
// the client did not expect, and the point of the exercise is to find out what
// that state IS.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8791", "loopback address to listen on")
	upstream := flag.String("upstream", "https://api.anthropic.com", "provider base URL")
	name := flag.String("shape", "", "candidate refusal shape to inject (required; -list to see them)")
	after := flag.Int64("after", 2, "how many model calls pass through before one is refused")
	path := flag.String("path", "/v1/messages", "request path suffix that qualifies")
	list := flag.Bool("list", false, "print the candidate shapes and exit")
	flag.Parse()

	if *list {
		for _, s := range Shapes {
			fmt.Printf("%-24s %d %-24s %s\n", s.Name, s.Status, s.ContentType, s.Retryable)
			fmt.Printf("%-24s    %s\n\n", "", s.Rationale)
		}
		return
	}
	if *name == "" {
		fmt.Fprintln(os.Stderr, "refusal-injector: -shape is required (try -list)")
		os.Exit(2)
	}
	shape, ok := ShapeByName(*name)
	if !ok {
		fmt.Fprintf(os.Stderr, "refusal-injector: unknown shape %q (try -list)\n", *name)
		os.Exit(2)
	}

	inj, err := NewInjector(*upstream, shape, *after, *path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "refusal-injector:", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "refusal-injector: listening on %s, forwarding to %s\n", *addr, *upstream)
	fmt.Fprintf(os.Stderr, "refusal-injector: will inject %q after %d %s call(s)\n", shape.Name, *after, *path)
	fmt.Fprintf(os.Stderr, "refusal-injector: THROWAWAY SESSIONS ONLY; this fabricates provider responses\n")
	fmt.Fprintf(os.Stderr, "refusal-injector: point the tool at it with ANTHROPIC_BASE_URL=http://%s\n", *addr)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           inj,
		ReadHeaderTimeout: 30 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "refusal-injector:", err)
		os.Exit(1)
	}
}
