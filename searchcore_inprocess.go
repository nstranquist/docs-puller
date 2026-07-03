//go:build docs_puller_searchcore_inprocess

package main

import (
	"context"

	"github.com/nstranquist/docs-puller/searchruntime"
)

func newInProcessSearchcoreAdapter(opts searchOpts) searchruntime.Searcher {
	return searchruntime.NewDispatchEngineSearcher(searchruntime.DispatchEngineConfig[searchOpts, *ftsIndex]{
		BaseOptions: searchruntime.Options{
			Limit:     opts.limit,
			Source:    opts.source,
			RerankLLM: opts.rerankLLM,
		},
		EngineOptions: opts,
		ApplyOptions: func(opts searchOpts, runtimeOpts searchruntime.Options) searchOpts {
			opts.limit = runtimeOpts.Limit
			opts.source = runtimeOpts.Source
			opts.rerankLLM = runtimeOpts.RerankLLM
			return opts
		},
		Dispatch: func(ctx context.Context, req searchruntime.DispatchRequest[searchOpts, *ftsIndex]) searchruntime.DispatchResult[searchruntime.Hit] {
			if err := ctx.Err(); err != nil {
				return searchruntime.DispatchResult[searchruntime.Hit]{}
			}
			return runDispatchSearch(req)
		},
	})
}
