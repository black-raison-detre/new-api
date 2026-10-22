package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
)

const (
	openRouterEndpointCatalogTTL        = 5 * time.Minute
	openRouterEndpointCatalogMaxEntries = 128
	openRouterEndpointCatalogMaxFlight  = 32
	openRouterEndpointCatalogTimeout    = 10 * time.Second
)

var ErrOpenRouterEndpointCatalogBusy = errors.New("OpenRouter endpoint discovery is busy")

type OpenRouterEndpoint struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	ProviderName string `json:"provider_name"`
}

type OpenRouterEndpointCatalog struct {
	Model     string               `json:"model"`
	Endpoints []OpenRouterEndpoint `json:"endpoints"`
	FetchedAt int64                `json:"fetched_at"`
	Stale     bool                 `json:"stale"`
}

// OpenRouterEndpointCatalogIdentity contains the channel settings that affect
// the upstream response. The cache hashes these values and never stores them.
type OpenRouterEndpointCatalogIdentity struct {
	BaseURL        string
	Key            string
	Proxy          string
	HeaderOverride string
}

type OpenRouterEndpointCatalogFetcher func(context.Context) ([]OpenRouterEndpoint, error)

type OpenRouterEndpointCatalogCache struct {
	mu       sync.Mutex
	entries  map[string]openRouterEndpointCatalogEntry
	inFlight map[string]*openRouterEndpointCatalogCall
	now      func() time.Time
	ttl      time.Duration
	max      int
	timeout  time.Duration
}

type openRouterEndpointCatalogEntry struct {
	catalog   OpenRouterEndpointCatalog
	expiresAt time.Time
	lastUsed  time.Time
}

type openRouterEndpointCatalogCall struct {
	done    chan struct{}
	catalog OpenRouterEndpointCatalog
	err     error
}

func NewOpenRouterEndpointCatalogCache() *OpenRouterEndpointCatalogCache {
	return &OpenRouterEndpointCatalogCache{
		entries:  make(map[string]openRouterEndpointCatalogEntry),
		inFlight: make(map[string]*openRouterEndpointCatalogCall),
		now:      time.Now,
		ttl:      openRouterEndpointCatalogTTL,
		max:      openRouterEndpointCatalogMaxEntries,
		timeout:  openRouterEndpointCatalogTimeout,
	}
}

var openRouterEndpointCatalogCache = NewOpenRouterEndpointCatalogCache()

func GetOpenRouterEndpointCatalog(
	ctx context.Context,
	identity OpenRouterEndpointCatalogIdentity,
	model string,
	fetch OpenRouterEndpointCatalogFetcher,
) (OpenRouterEndpointCatalog, error) {
	return openRouterEndpointCatalogCache.Get(ctx, identity, model, fetch)
}

func (cache *OpenRouterEndpointCatalogCache) Get(
	ctx context.Context,
	identity OpenRouterEndpointCatalogIdentity,
	model string,
	fetch OpenRouterEndpointCatalogFetcher,
) (OpenRouterEndpointCatalog, error) {
	if fetch == nil {
		return OpenRouterEndpointCatalog{}, errors.New("OpenRouter endpoint fetcher is required")
	}
	key, err := hashOpenRouterEndpointCatalogIdentity(identity, model)
	if err != nil {
		return OpenRouterEndpointCatalog{}, err
	}

	now := cache.now()
	cache.mu.Lock()
	entry, hasEntry := cache.entries[key]
	if hasEntry && entry.expiresAt.After(now) {
		entry.lastUsed = now
		cache.entries[key] = entry
		catalog := cloneOpenRouterEndpointCatalog(entry.catalog)
		cache.mu.Unlock()
		return catalog, nil
	}
	if call, ok := cache.inFlight[key]; ok {
		cache.mu.Unlock()
		select {
		case <-ctx.Done():
			return OpenRouterEndpointCatalog{}, ctx.Err()
		case <-call.done:
			return cloneOpenRouterEndpointCatalog(call.catalog), call.err
		}
	}
	if len(cache.inFlight) >= openRouterEndpointCatalogMaxFlight {
		cache.mu.Unlock()
		if hasEntry {
			stale := cloneOpenRouterEndpointCatalog(entry.catalog)
			stale.Stale = true
			return stale, nil
		}
		return OpenRouterEndpointCatalog{}, ErrOpenRouterEndpointCatalogBusy
	}
	call := &openRouterEndpointCatalogCall{done: make(chan struct{})}
	cache.inFlight[key] = call
	cache.mu.Unlock()

	fetchCtx, cancel := context.WithTimeout(ctx, cache.timeout)
	endpoints, fetchErr := fetch(fetchCtx)
	cancel()
	if fetchErr == nil {
		endpoints = cloneOpenRouterEndpoints(endpoints)
	}

	now = cache.now()
	cache.mu.Lock()
	delete(cache.inFlight, key)
	if fetchErr == nil {
		catalog := OpenRouterEndpointCatalog{
			Model:     model,
			Endpoints: endpoints,
			FetchedAt: now.Unix(),
		}
		cache.evictIfFullLocked()
		cache.entries[key] = openRouterEndpointCatalogEntry{
			catalog:   cloneOpenRouterEndpointCatalog(catalog),
			expiresAt: now.Add(cache.ttl),
			lastUsed:  now,
		}
		call.catalog = catalog
	} else if hasEntry {
		call.catalog = cloneOpenRouterEndpointCatalog(entry.catalog)
		call.catalog.Stale = true
		call.err = nil
	} else {
		call.err = fetchErr
	}
	close(call.done)
	cache.mu.Unlock()

	return cloneOpenRouterEndpointCatalog(call.catalog), call.err
}

func hashOpenRouterEndpointCatalogIdentity(identity OpenRouterEndpointCatalogIdentity, model string) (string, error) {
	material, err := common.Marshal(struct {
		BaseURL        string `json:"base_url"`
		Key            string `json:"key"`
		Proxy          string `json:"proxy"`
		HeaderOverride string `json:"header_override"`
		Model          string `json:"model"`
	}{
		BaseURL:        identity.BaseURL,
		Key:            identity.Key,
		Proxy:          identity.Proxy,
		HeaderOverride: identity.HeaderOverride,
		Model:          model,
	})
	if err != nil {
		return "", fmt.Errorf("hash OpenRouter endpoint catalog identity: %w", err)
	}
	digest := sha256.Sum256(material)
	return hex.EncodeToString(digest[:]), nil
}

func (cache *OpenRouterEndpointCatalogCache) evictIfFullLocked() {
	if len(cache.entries) < cache.max {
		return
	}
	var oldestKey string
	var oldest time.Time
	for key, entry := range cache.entries {
		if oldestKey == "" || entry.lastUsed.Before(oldest) {
			oldestKey = key
			oldest = entry.lastUsed
		}
	}
	delete(cache.entries, oldestKey)
}

func cloneOpenRouterEndpointCatalog(catalog OpenRouterEndpointCatalog) OpenRouterEndpointCatalog {
	catalog.Endpoints = cloneOpenRouterEndpoints(catalog.Endpoints)
	return catalog
}

func cloneOpenRouterEndpoints(endpoints []OpenRouterEndpoint) []OpenRouterEndpoint {
	if endpoints == nil {
		return nil
	}
	cloned := make([]OpenRouterEndpoint, len(endpoints))
	copy(cloned, endpoints)
	return cloned
}
