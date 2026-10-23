package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenRouterEndpointCatalogCacheSeparatesKeysAndDoesNotStoreSecrets(t *testing.T) {
	cache := NewOpenRouterEndpointCatalogCache()
	savedIdentity := OpenRouterEndpointCatalogIdentity{BaseURL: "https://openrouter.ai/api", Key: "saved-secret"}
	previewIdentity := savedIdentity
	previewIdentity.Key = "preview-secret"
	fetches := 0

	saved, err := cache.Get(context.Background(), savedIdentity, "openai/gpt-4.1-nano:nitro", func(context.Context) ([]OpenRouterEndpoint, error) {
		fetches++
		return []OpenRouterEndpoint{{ID: "saved-provider", Name: "Saved", ProviderName: "Saved Provider"}}, nil
	})
	require.NoError(t, err)
	require.Equal(t, "saved-provider", saved.Endpoints[0].ID)

	preview, err := cache.Get(context.Background(), previewIdentity, "openai/gpt-4.1-nano:nitro", func(context.Context) ([]OpenRouterEndpoint, error) {
		fetches++
		return []OpenRouterEndpoint{{ID: "preview-provider", Name: "Preview", ProviderName: "Preview Provider"}}, nil
	})
	require.NoError(t, err)
	require.Equal(t, "preview-provider", preview.Endpoints[0].ID)
	require.Equal(t, 2, fetches)
	require.Len(t, cache.entries, 2)
	for key := range cache.entries {
		require.NotContains(t, key, "saved-secret")
		require.NotContains(t, key, "preview-secret")
		require.Len(t, key, 64)
	}
}

func TestOpenRouterEndpointCatalogCacheTimesOutAndReturnsLastGoodData(t *testing.T) {
	cache := NewOpenRouterEndpointCatalogCache()
	cache.timeout = 5 * time.Millisecond
	now := time.Now()
	cache.now = func() time.Time { return now }
	identity := OpenRouterEndpointCatalogIdentity{BaseURL: "https://openrouter.ai/api", Key: "key"}
	fetchUntilCanceled := func(ctx context.Context) ([]OpenRouterEndpoint, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	_, err := cache.Get(context.Background(), identity, "openai/gpt-4.1", fetchUntilCanceled)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	cache.timeout = time.Second
	lastGood, err := cache.Get(context.Background(), identity, "openai/gpt-4.1", func(context.Context) ([]OpenRouterEndpoint, error) {
		return []OpenRouterEndpoint{{ID: "openai", Name: "OpenAI: GPT-4.1", ProviderName: "OpenAI"}}, nil
	})
	require.NoError(t, err)
	require.False(t, lastGood.Stale)
	now = now.Add(cache.ttl + time.Second)
	cache.timeout = 5 * time.Millisecond

	stale, err := cache.Get(context.Background(), identity, "openai/gpt-4.1", fetchUntilCanceled)
	require.NoError(t, err)
	require.True(t, stale.Stale)
	require.Equal(t, lastGood.FetchedAt, stale.FetchedAt)
	require.Equal(t, lastGood.Endpoints, stale.Endpoints)

	stale, err = cache.Get(context.Background(), identity, "openai/gpt-4.1", func(context.Context) ([]OpenRouterEndpoint, error) {
		return nil, errors.New("upstream unavailable")
	})
	require.NoError(t, err)
	require.True(t, stale.Stale)
	require.Equal(t, lastGood.Endpoints, stale.Endpoints)
}

func TestOpenRouterEndpointCatalogCacheBoundsRetainedEntries(t *testing.T) {
	cache := NewOpenRouterEndpointCatalogCache()
	cache.max = 1
	for _, key := range []string{"first", "second"} {
		_, err := cache.Get(context.Background(), OpenRouterEndpointCatalogIdentity{Key: key}, "openai/gpt-4.1", func(context.Context) ([]OpenRouterEndpoint, error) {
			return []OpenRouterEndpoint{{ID: strings.ToLower(key), Name: key, ProviderName: key}}, nil
		})
		require.NoError(t, err)
	}
	require.Len(t, cache.entries, 1)
}

func TestOpenRouterEndpointCatalogCacheClonesNestedMetadata(t *testing.T) {
	cache := NewOpenRouterEndpointCatalogCache()
	identity := OpenRouterEndpointCatalogIdentity{Key: "saved-secret"}
	metadata := common.RawMessage(`{"pricing":null,"latency":null,"future":{"values":[1,2]}}`)
	fetches := 0

	first, err := cache.Get(context.Background(), identity, "openai/gpt-4.1", func(context.Context) ([]OpenRouterEndpoint, error) {
		fetches++
		return []OpenRouterEndpoint{{ID: "openai", Name: "OpenAI", ProviderName: "OpenAI", Metadata: metadata}}, nil
	})
	require.NoError(t, err)
	require.Len(t, first.Endpoints, 1)
	wantMetadata := string(metadata)
	metadata[0] = ' '
	first.Endpoints[0].Metadata[1] = ' '

	second, err := cache.Get(context.Background(), identity, "openai/gpt-4.1", func(context.Context) ([]OpenRouterEndpoint, error) {
		fetches++
		return nil, errors.New("cache miss")
	})
	require.NoError(t, err)
	require.Len(t, second.Endpoints, 1)
	assert.Equal(t, wantMetadata, string(second.Endpoints[0].Metadata))
	assert.Equal(t, 1, fetches)
}

func TestOpenRouterModelCatalogCachePreservesRawMetadataAndClonesResults(t *testing.T) {
	cache := NewOpenRouterModelCatalogCache()
	now := time.Unix(1_800_000_000, 0)
	cache.now = func() time.Time { return now }
	identity := OpenRouterCatalogIdentity{BaseURL: "https://openrouter.ai/api", Key: "catalog-secret"}
	metadata := common.RawMessage(`{"id":"openai/gpt-4.1","pricing":null,"latency":null,"throughput":null,"uptime":null,"future":{"values":[1,2]}}`)
	wantMetadata := string(metadata)
	fetches := 0

	first, err := cache.Get(context.Background(), identity, "public", func(context.Context) ([]OpenRouterCatalogModel, error) {
		fetches++
		return []OpenRouterCatalogModel{{ID: "openai/gpt-4.1", Metadata: metadata}}, nil
	})
	require.NoError(t, err)
	require.Len(t, first.Models, 1)
	assert.Equal(t, "public", first.Scope)
	assert.Equal(t, "openrouter", first.Source)
	assert.Equal(t, now.Unix(), first.FetchedAt)
	assert.Equal(t, wantMetadata, string(first.Models[0].Metadata))

	metadata[0] = ' '
	first.Models[0].Metadata[1] = ' '
	second, err := cache.Get(context.Background(), identity, "public", func(context.Context) ([]OpenRouterCatalogModel, error) {
		fetches++
		return nil, errors.New("cache miss")
	})
	require.NoError(t, err)
	require.Len(t, second.Models, 1)
	assert.Equal(t, wantMetadata, string(second.Models[0].Metadata))
	assert.Equal(t, 1, fetches)
}

func TestOpenRouterModelCatalogCacheRejectsUnknownScope(t *testing.T) {
	cache := NewOpenRouterModelCatalogCache()
	for _, scope := range []string{"", "Public", "private", "account "} {
		_, err := cache.Get(context.Background(), OpenRouterCatalogIdentity{Key: "catalog-secret"}, scope, func(context.Context) ([]OpenRouterCatalogModel, error) {
			return nil, nil
		})
		require.ErrorIs(t, err, ErrOpenRouterModelCatalogInvalidScope)
	}
}

func TestOpenRouterModelCatalogCacheSeparatesScopeAndSecrets(t *testing.T) {
	cache := NewOpenRouterModelCatalogCache()
	identity := OpenRouterCatalogIdentity{BaseURL: "https://openrouter.ai/api", Key: "saved-secret"}
	otherIdentity := identity
	otherIdentity.Key = "account-secret"
	fetches := 0

	fetch := func(context.Context) ([]OpenRouterCatalogModel, error) {
		fetches++
		return []OpenRouterCatalogModel{{ID: strings.ToLower(string(rune('A' + fetches - 1)))}}, nil
	}
	public, err := cache.Get(context.Background(), identity, "public", fetch)
	require.NoError(t, err)
	publicCached, err := cache.Get(context.Background(), identity, "public", fetch)
	require.NoError(t, err)
	assert.Equal(t, public.Models, publicCached.Models)
	account, err := cache.Get(context.Background(), identity, "account", fetch)
	require.NoError(t, err)
	otherKey, err := cache.Get(context.Background(), otherIdentity, "public", fetch)
	require.NoError(t, err)
	assert.Equal(t, 3, fetches)
	assert.NotEqual(t, public.Models, account.Models)
	assert.NotEqual(t, public.Models, otherKey.Models)
	assert.Len(t, cache.entries, 3)
	for key := range cache.entries {
		assert.NotContains(t, key, "saved-secret")
		assert.NotContains(t, key, "account-secret")
		assert.Len(t, key, 64)
	}
}

func TestOpenRouterModelCatalogCacheDeadlineAndStaleFallbackPreserveFetchTime(t *testing.T) {
	cache := NewOpenRouterModelCatalogCache()
	now := time.Unix(1_800_000_000, 0)
	cache.now = func() time.Time { return now }
	cache.timeout = 0
	identity := OpenRouterCatalogIdentity{Key: "catalog-secret"}
	failOnDeadline := func(ctx context.Context) ([]OpenRouterCatalogModel, error) {
		return nil, ctx.Err()
	}
	_, err := cache.Get(context.Background(), identity, "public", failOnDeadline)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	cache.timeout = time.Second
	fresh, err := cache.Get(context.Background(), identity, "public", func(context.Context) ([]OpenRouterCatalogModel, error) {
		return []OpenRouterCatalogModel{{ID: "openai/gpt-4.1", Metadata: common.RawMessage(`{"pricing":null}`)}}, nil
	})
	require.NoError(t, err)
	assert.False(t, fresh.Stale)
	now = now.Add(cache.ttl + time.Second)
	cache.timeout = 0
	stale, err := cache.Get(context.Background(), identity, "public", failOnDeadline)
	require.NoError(t, err)
	assert.True(t, stale.Stale)
	assert.Equal(t, fresh.FetchedAt, stale.FetchedAt)
	assert.Equal(t, fresh.Models, stale.Models)
}

func TestOpenRouterModelCatalogCacheCoalescesRequestsAndBoundsWork(t *testing.T) {
	cache := NewOpenRouterModelCatalogCache()
	identity := OpenRouterCatalogIdentity{Key: "shared-secret"}
	fetchStarted := make(chan struct{})
	releaseFetch := make(chan struct{})
	var fetches atomic.Int32
	fetch := func(ctx context.Context) ([]OpenRouterCatalogModel, error) {
		fetches.Add(1)
		close(fetchStarted)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-releaseFetch:
			return []OpenRouterCatalogModel{{ID: "openai/gpt-4.1"}}, nil
		}
	}
	type result struct {
		catalog OpenRouterModelCatalog
		err     error
	}
	firstDone := make(chan result, 1)
	go func() {
		catalog, err := cache.Get(context.Background(), identity, "public", fetch)
		firstDone <- result{catalog: catalog, err: err}
	}()
	<-fetchStarted

	followerContext, cancelFollower := context.WithCancel(context.Background())
	followerDone := make(chan result, 1)
	go func() {
		catalog, err := cache.Get(followerContext, identity, "public", func(context.Context) ([]OpenRouterCatalogModel, error) {
			fetches.Add(1)
			return nil, errors.New("same-key request was not coalesced")
		})
		followerDone <- result{catalog: catalog, err: err}
	}()
	cancelFollower()
	follower := <-followerDone
	require.ErrorIs(t, follower.err, context.Canceled)
	assert.EqualValues(t, 1, fetches.Load())

	// An uncancelled caller must receive the same result without fetching again.
	successfulFollowerStarted := make(chan struct{})
	successfulFollowerDone := make(chan result, 1)
	go func() {
		close(successfulFollowerStarted)
		catalog, err := cache.Get(context.Background(), identity, "public", func(context.Context) ([]OpenRouterCatalogModel, error) {
			fetches.Add(1)
			return nil, errors.New("duplicate catalog fetch")
		})
		successfulFollowerDone <- result{catalog: catalog, err: err}
	}()
	<-successfulFollowerStarted
	close(releaseFetch)
	first := <-firstDone
	require.NoError(t, first.err)
	require.Len(t, first.catalog.Models, 1)
	assert.Equal(t, "openai/gpt-4.1", first.catalog.Models[0].ID)
	successfulFollower := <-successfulFollowerDone
	require.NoError(t, successfulFollower.err)
	assert.Equal(t, first.catalog, successfulFollower.catalog)
	assert.EqualValues(t, 1, fetches.Load())

	boundedCache := NewOpenRouterModelCatalogCache()
	boundedCache.maxFlight = 1
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	workDone := make(chan error, 1)
	var wait sync.WaitGroup
	wait.Go(func() {
		_, err := boundedCache.Get(context.Background(), OpenRouterCatalogIdentity{Key: "first"}, "public", func(context.Context) ([]OpenRouterCatalogModel, error) {
			started <- struct{}{}
			<-release
			return []OpenRouterCatalogModel{{ID: "first"}}, nil
		})
		workDone <- err
	})
	<-started
	wasCalled := false
	_, err := boundedCache.Get(context.Background(), OpenRouterCatalogIdentity{Key: "overflow"}, "public", func(context.Context) ([]OpenRouterCatalogModel, error) {
		wasCalled = true
		return nil, nil
	})
	require.ErrorIs(t, err, ErrOpenRouterModelCatalogBusy)
	assert.False(t, wasCalled)
	close(release)
	require.NoError(t, <-workDone)
	wait.Wait()
}

func TestOpenRouterModelCatalogCacheBoundsRetainedEntries(t *testing.T) {
	cache := NewOpenRouterModelCatalogCache()
	for i := range openRouterModelCatalogMaxEntries + 1 {
		_, err := cache.Get(context.Background(), OpenRouterCatalogIdentity{Key: strings.ToLower(string(rune('A' + i)))}, "public", func(context.Context) ([]OpenRouterCatalogModel, error) {
			return []OpenRouterCatalogModel{{ID: "model"}}, nil
		})
		require.NoError(t, err)
	}
	assert.Len(t, cache.entries, openRouterModelCatalogMaxEntries)
}
