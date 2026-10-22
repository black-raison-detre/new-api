package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
