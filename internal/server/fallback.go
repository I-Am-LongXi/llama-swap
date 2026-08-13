package server

import (
	"net/http"
	"sort"

	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// CreateFallbackMiddleware routes requests naming an unknown model to a model
// that is already running instead of rejecting them with a 404. It runs after
// the profile and selector rewrites so those always take precedence, and before
// the request context so the rewritten model is what gets resolved.
//
// When nothing is running the request falls through untouched and
// CreateRequestContextMiddleware produces the usual 404.
func CreateFallbackMiddleware(s *Server) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			model, err := swaputil.ExtractModel(r)
			if err != nil || model == "" {
				next.ServeHTTP(w, r)
				return
			}

			// Configured local models, aliases and peer models are all real
			// targets and must never be redirected.
			if _, found := s.cfg.ResolveBaseModel(model); found {
				next.ServeHTTP(w, r)
				return
			}

			target, found := activeModel(s.local.RunningModels())
			if !found {
				next.ServeHTTP(w, r)
				return
			}

			updated, err := swaputil.ReplaceRequestModel(r, model, target)
			if err != nil {
				swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
				return
			}

			s.proxylog.Infof("fallback: model %q not configured, routing to active model %q", model, target)
			next.ServeHTTP(w, updated)
		})
	}
}

// activeModel picks the model to fall back to, preferring one that is ready to
// serve over one that is still loading. Names are sorted so the choice is
// stable when several models share the winning state.
func activeModel(running map[string]process.ProcessState) (string, bool) {
	for _, state := range []process.ProcessState{process.StateReady, process.StateStarting} {
		candidates := make([]string, 0, len(running))
		for modelID, st := range running {
			if st == state {
				candidates = append(candidates, modelID)
			}
		}
		if len(candidates) > 0 {
			sort.Strings(candidates)
			return candidates[0], true
		}
	}
	return "", false
}
