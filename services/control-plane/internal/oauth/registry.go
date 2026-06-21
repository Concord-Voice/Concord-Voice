package oauth

import "fmt"

// Registry maps provider names to implementations. Not safe for concurrent
// modification — populate at server startup, then treat as read-only.
type Registry struct {
	providers map[string]Provider
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{providers: make(map[string]Provider)}
}

// Register adds or replaces a provider in the registry. Replacement is
// intentional: tests can register fakes that override real providers.
func (r *Registry) Register(p Provider) {
	r.providers[p.Name()] = p
}

// Get returns the provider registered under name, or an error if none.
func (r *Registry) Get(name string) (Provider, error) {
	p, ok := r.providers[name]
	if !ok {
		return nil, fmt.Errorf("oauth: unknown provider %q", name)
	}
	return p, nil
}
