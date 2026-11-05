package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/nstranquist/docs-puller/evidence"
)

func cmdContext(args []string) {
	o := defaultOpts()
	fs := flag.NewFlagSet("context", flag.ExitOnError)
	out := fs.String("out", o.out, "corpus directory")
	source := fs.String("source", "", "restrict retrieval to one source")
	path := fs.String("path", "", "read one corpus-relative Markdown path instead of searching")
	line := fs.Int("line", 1, "first requested line for --path")
	lines := fs.Int("lines", 60, "requested lines per passage; fenced blocks remain whole")
	limit := fs.Int("limit", 5, "maximum search results")
	budget := fs.Int("max-bytes", 16000, "maximum passage text bytes; excludes JSON metadata")
	fs.Bool("json", true, "emit the versioned evidence envelope")
	flags, words := splitContextArgs(args)
	fs.Parse(flags)
	if *lines < 1 || *lines > 10000 || *limit < 1 || *limit > 100 || *line < 1 || *budget < 1 || *budget > 4<<20 {
		die(fmt.Errorf("invalid context limits: lines 1..10000, limit 1..100, line >=1, max-bytes 1..4194304"))
	}
	requests := []evidence.Request{}
	if *path != "" {
		requests = append(requests, evidence.Request{Path: *path, Line: *line, Lines: *lines})
	} else {
		query := strings.TrimSpace(strings.Join(words, " "))
		if query == "" {
			die(fmt.Errorf("context requires a query or --path"))
		}
		opts := searchOpts{out: *out, source: *source, limit: *limit}
		resolveSearchProfile(&opts)
		hits, _, _ := dispatchSearch(query, opts, nil)
		for _, hit := range hits {
			l := 1
			if len(hit.Snippets) > 0 {
				l = max(1, hit.Snippets[0].Line-5)
			}
			requests = append(requests, evidence.Request{Path: hit.Path, URL: hit.URL, Line: l, Lines: *lines})
		}
	}
	pack, err := evidence.Build(context.Background(), *out, requests, *budget)
	if err != nil {
		die(err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(pack); err != nil {
		die(err)
	}
}

// Context has value flags not recognized by the historical search splitter.
func splitContextArgs(args []string) (flags, words []string) {
	values := map[string]bool{"out": true, "source": true, "path": true, "line": true, "lines": true, "limit": true, "max-bytes": true}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			words = append(words, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") {
			words = append(words, a)
			continue
		}
		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if !strings.Contains(name, "=") && values[name] && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return
}
