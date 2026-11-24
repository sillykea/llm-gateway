// Package router turns a model alias such as "chat-default" into an ordered
// list of provider/model targets to try.
package router

import (
	"fmt"

	"github.com/sillykea/llm-gateway/internal/config"
	"github.com/sillykea/llm-gateway/internal/provider"
)

// Target is a resolved provider plus the model name to send it.
type Target struct {
	Provider provider.Provider
	Model    string
}

// Router holds the alias table.
type Router struct {
	routes map[string][]Target
}

// New wires model aliases to provider instances.
func New(models map[string][]config.Target, providers map[string]provider.Provider) (*Router, error) {
	r := &Router{routes: map[string][]Target{}}
	for alias, targets := range models {
		for _, t := range targets {
			p, ok := providers[t.Provider]
			if !ok {
				return nil, fmt.Errorf("model %q: unknown provider %q", alias, t.Provider)
			}
			r.routes[alias] = append(r.routes[alias], Target{Provider: p, Model: t.Model})
		}
	}
	return r, nil
}

// Resolve returns the targets for an alias in fallback order.
// If localOnly is set, only providers running on our own hardware are kept.
func (r *Router) Resolve(alias string, localOnly bool) ([]Target, bool) {
	targets, ok := r.routes[alias]
	if !ok {
		return nil, false
	}
	if !localOnly {
		return targets, true
	}
	var local []Target
	for _, t := range targets {
		if t.Provider.Local() {
			local = append(local, t)
		}
	}
	return local, true
}

// Aliases lists the configured model aliases (used by GET /v1/models).
func (r *Router) Aliases() []string {
	out := make([]string, 0, len(r.routes))
	for a := range r.routes {
		out = append(out, a)
	}
	return out
}
