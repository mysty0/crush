package config

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/catwalk/pkg/embedded"
	"github.com/charmbracelet/crush/internal/agent/hyper"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/discover"
	"github.com/charmbracelet/crush/internal/home"
	"github.com/charmbracelet/x/etag"
)

type syncer[T any] interface {
	Get(context.Context) (T, error)
}

var (
	providerOnce sync.Once
	providerList []catwalk.Provider
	providerErr  error
)

// file to cache provider data
func cachePathFor(name string) string {
	xdgDataHome := os.Getenv("XDG_DATA_HOME")
	if xdgDataHome != "" {
		return filepath.Join(xdgDataHome, appName, name+".json")
	}

	// return the path to the main data directory
	// for windows, it should be in `%LOCALAPPDATA%/crush/`
	// for linux and macOS, it should be in `$HOME/.local/share/crush/`
	if runtime.GOOS == "windows" {
		localAppData := os.Getenv("LOCALAPPDATA")
		if localAppData == "" {
			localAppData = filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Local")
		}
		return filepath.Join(localAppData, appName, name+".json")
	}

	return filepath.Join(home.Dir(), ".local", "share", appName, name+".json")
}

// UpdateProviders updates the Catwalk providers list from a specified source.
func UpdateProviders(pathOrURL string) error {
	var providers []catwalk.Provider
	pathOrURL = cmp.Or(pathOrURL, os.Getenv("CATWALK_URL"), defaultCatwalkURL)

	switch {
	case pathOrURL == "embedded":
		providers = embedded.GetAll()
	case strings.HasPrefix(pathOrURL, "http://") || strings.HasPrefix(pathOrURL, "https://"):
		var err error
		providers, err = catwalk.NewWithURL(pathOrURL).GetProviders(context.Background(), "")
		if err != nil {
			return fmt.Errorf("failed to fetch providers from Catwalk: %w", err)
		}
	default:
		content, err := os.ReadFile(pathOrURL)
		if err != nil {
			return fmt.Errorf("failed to read file: %w", err)
		}
		if err := json.Unmarshal(content, &providers); err != nil {
			return fmt.Errorf("failed to unmarshal provider data: %w", err)
		}
		if len(providers) == 0 {
			return fmt.Errorf("no providers found in the provided source")
		}
	}

	if err := newCache[[]catwalk.Provider](cachePathFor("providers")).Store(providers); err != nil {
		return fmt.Errorf("failed to save providers to cache: %w", err)
	}

	slog.Info("Providers updated successfully", "count", len(providers), "from", pathOrURL, "to", cachePathFor)
	return nil
}

// resolveHyperAPIKey returns the Hyper API key from the environment or
// the raw config value. The env var takes precedence.
func resolveHyperAPIKey(cfg *Config) string {
	if key := os.Getenv("HYPER_API_KEY"); key != "" {
		return key
	}
	if cfg == nil || cfg.Providers == nil {
		return ""
	}
	pc, ok := cfg.Providers.Get("hyper")
	if !ok {
		return ""
	}
	return pc.APIKey
}

// HyperTokenRefresher is a function that refreshes the Hyper OAuth
// token. It is passed to Providers so the catalog fetch can retry on
// 401 without relying on package-global state.
type HyperTokenRefresher func(context.Context) error

// UpdateHyper updates the Hyper provider information from a specified URL.
func UpdateHyper(pathOrURL string) error {
	var provider catwalk.Provider
	pathOrURL = cmp.Or(pathOrURL, hyper.BaseURL())

	switch {
	case pathOrURL == "embedded":
		provider = hyper.Embedded()
	case strings.HasPrefix(pathOrURL, "http://") || strings.HasPrefix(pathOrURL, "https://"):
		client := realHyperClient{
			baseURL:    pathOrURL,
			resolveKey: func() string { return resolveHyperAPIKey(nil) },
		}
		var err error
		provider, err = client.Get(context.Background(), "")
		if err != nil {
			return fmt.Errorf("failed to fetch provider from Hyper: %w", err)
		}
	default:
		content, err := os.ReadFile(pathOrURL)
		if err != nil {
			return fmt.Errorf("failed to read file: %w", err)
		}
		if err := json.Unmarshal(content, &provider); err != nil {
			return fmt.Errorf("failed to unmarshal provider data: %w", err)
		}
	}

	if err := newCache[catwalk.Provider](cachePathFor("hyper")).Store(provider); err != nil {
		return fmt.Errorf("failed to save Hyper provider to cache: %w", err)
	}

	slog.Info("Hyper provider updated successfully", "from", pathOrURL, "to", cachePathFor("hyper"))
	return nil
}

var (
	catwalkSyncer = &catwalkSync{}
	hyperSyncer   = &hyperSync{}
)

// providersFast returns the known-provider catalog without making any
// network calls: the on-disk cache if present, otherwise the embedded
// catalog bundled with this release (plus a cached Hyper entry, if
// any). Load uses this to build a usable config instantly on every
// launch; refreshProvidersInBackground fetches the live catalog
// afterward and merges it into the running config store.
func providersFast(cfg *Config) []catwalk.Provider {
	if cfg.Options.DisableDefaultProviders {
		return nil
	}
	out := embedded.GetAll()
	if cached, _, err := newCache[[]catwalk.Provider](cachePathFor("providers")).Get(); err == nil && len(cached) > 0 {
		out = cached
	}
	if hp, _, err := newCache[catwalk.Provider](cachePathFor("hyper")).Get(); err == nil && hp.ID != "" {
		out = append([]catwalk.Provider{hp}, out...)
	}
	return out
}

// refreshProvidersInBackground fetches the live provider catalog (the
// same network call Providers makes) and, once it lands, updates
// store's known-provider list so later reads (e.g. the model switcher,
// which calls Providers directly) see the fresh catalog. It never
// blocks the caller.
func refreshProvidersInBackground(store *ConfigStore, refresher HyperTokenRefresher) {
	// Skipped under test: package-level state (providerOnce, catwalkSyncer,
	// hyperSyncer) is reset directly by test helpers between cases, and a
	// goroutine outliving its originating test would race those resets.
	if testing.Testing() || store.Config().Options.DisableProviderAutoUpdate {
		return
	}
	go func() {
		fresh, err := Providers(store.Config(), refresher)
		if len(fresh) == 0 {
			if err != nil {
				slog.Warn("Background provider catalog refresh failed", "error", err)
			}
			return
		}
		store.writeMu.Lock()
		store.knownProviders = fresh
		store.writeMu.Unlock()
	}()
}

// customModelsCachePath returns the on-disk cache path for a custom
// provider's last successfully discovered model list, keyed by the
// provider's config ID and a hash of its endpoint so that pointing the
// same provider ID at a different BaseURL (or API key) never serves a
// stale list discovered from somewhere else.
func customModelsCachePath(providerID, baseURL, apiKey string) string {
	sum := sha256.Sum256([]byte(baseURL + "\x00" + apiKey))
	return cachePathFor(fmt.Sprintf("discover-%s-%x", providerID, sum[:8]))
}

// refreshCustomModelsInBackground re-runs model discovery for a custom
// provider that already has a usable on-disk result and merges the
// outcome into the live config store once it lands, without blocking
// the caller. Used so a provider Load has seen before never blocks
// startup on a fresh discovery call; see the discovery loop in
// configureProviders.
func refreshCustomModelsInBackground(store *ConfigStore, providerID, baseURL, apiKey string, fetch func(ctx context.Context) ([]catwalk.Model, error)) {
	// Skipped under test: same rationale as refreshProvidersInBackground.
	if testing.Testing() {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		models, err := fetch(ctx)
		if err != nil || len(models) == 0 {
			return
		}
		_ = newCache[[]catwalk.Model](customModelsCachePath(providerID, baseURL, apiKey)).Store(models)
		store.mutateInMemory(func(nc *Config) {
			if pc, ok := nc.Providers.Get(providerID); ok {
				pc.Models = models
				nc.Providers.Set(providerID, pc)
			}
		})
		store.SetupAgents()
	}()
}

// discoverCustomProviderInBackground runs first-time model discovery for
// a custom provider that has no cached success to fall back on --
// including one that has never succeeded, e.g. because of a bad API key
// or an unreachable/broken endpoint. Load never waits on this: the
// provider is simply absent from the running config until discovery
// succeeds (or forever, if it never does), at which point it is
// spliced into the live store. This is what keeps a single slow or
// permanently broken custom provider from adding its full discovery
// latency to every launch, forever, since a failing call is never
// cached.
func discoverCustomProviderInBackground(store *ConfigStore, id string, pc ProviderConfig, fetch func(ctx context.Context) ([]catwalk.Model, error)) {
	// Skipped under test: configureProviders discovers synchronously
	// under testing.Testing() and never calls this; guarded again here
	// so a direct call from a future test does not leak a goroutine.
	if testing.Testing() {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		models, err := fetch(ctx)
		if err != nil || len(models) == 0 {
			return
		}
		_ = newCache[[]catwalk.Model](customModelsCachePath(id, pc.BaseURL, pc.APIKey)).Store(models)

		resolver := store.Resolver()
		if resolver == nil {
			return
		}
		prepared, ok := prepareDiscoveredProvider(id, pc, models, resolver)
		if !ok {
			return
		}
		store.mutateInMemory(func(nc *Config) {
			nc.Providers.Set(id, prepared)
		})
		store.SetupAgents()
	}()
}

// prepareDiscoveredProvider fills in the same defaults and runs the same
// checks as the synchronous custom-provider validation loop in
// configureProviders, for the single provider that just finished a
// background discovery. It is intentionally a separate, minimal
// implementation rather than a shared call with that loop: the loop's
// control flow and log messages are covered by tests that assume
// synchronous discovery, and this rare, best-effort late-arrival path
// should not have to stay in lockstep with it.
func prepareDiscoveredProvider(id string, pc ProviderConfig, models []catwalk.Model, resolver VariableResolver) (ProviderConfig, bool) {
	pc.ID = id
	pc.Name = cmp.Or(pc.Name, id)
	pc.Type = cmp.Or(pc.Type, catwalk.TypeOpenAICompat)
	if pc.Disable || pc.BaseURL == "" || len(models) == 0 {
		return pc, false
	}
	if !slices.Contains(catwalk.KnownProviderTypes(), pc.Type) &&
		pc.Type != hyper.Name &&
		!discover.IsKnownCustomProvider(string(pc.Type)) {
		return pc, false
	}
	baseURL, err := resolver.ResolveValue(pc.BaseURL)
	if baseURL == "" || err != nil {
		return pc, false
	}
	pc.Models = models
	headers := make(map[string]string, len(pc.ExtraHeaders))
	for k, v := range pc.ExtraHeaders {
		resolved, err := resolver.ResolveValue(v)
		if err != nil || resolved == "" {
			continue
		}
		headers[k] = resolved
	}
	pc.ExtraHeaders = headers
	return pc, true
}

// Providers returns the list of providers, taking into account cached results
// and whether or not auto update is enabled.
//
// It will:
// 1. if auto update is disabled, it'll return the embedded providers at the
// time of release.
// 2. load the cached providers
// 3. try to get the fresh list of providers, and return either this new list,
// the cached list, or the embedded list if all others fail.
//
// A returned error is advisory: it reports that the catalog could not be
// cached, or that an upstream returned nothing usable. It never means that no
// providers are available, so callers should surface it as a warning and keep
// using the returned list. A refresh that simply could not reach the network
// is not an error at all: the cached or embedded catalog is a sound answer, so
// those are logged and the fallback is returned.
func Providers(cfg *Config, opts ...HyperTokenRefresher) ([]catwalk.Provider, error) {
	providerOnce.Do(func() {
		var wg sync.WaitGroup
		providers := csync.NewSlice[catwalk.Provider]()
		autoupdate := !cfg.Options.DisableProviderAutoUpdate
		customProvidersOnly := cfg.Options.DisableDefaultProviders

		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()

		// Each goroutine owns its own error so the two can report
		// independently without racing on a shared slice.
		var catwalkErr, hyperErr error
		var hyperProvider catwalk.Provider

		wg.Go(func() {
			if customProvidersOnly {
				return
			}
			catwalkURL := cmp.Or(os.Getenv("CATWALK_URL"), defaultCatwalkURL)
			client := catwalk.NewWithURL(catwalkURL)
			path := cachePathFor("providers")
			catwalkSyncer.Init(client, path, autoupdate)

			// A failure to refresh or cache the catalog is worth
			// reporting, but the syncer still hands back the cached or
			// embedded list. Dropping that would leave the user with no
			// providers at all over a transient disk or network problem.
			items, err := catwalkSyncer.Get(ctx)
			if err != nil {
				catwalkURL := fmt.Sprintf("%s/v2/providers", cmp.Or(os.Getenv("CATWALK_URL"), defaultCatwalkURL))
				catwalkErr = fmt.Errorf("Crush was unable to fetch an updated list of providers from %s. Consider setting CRUSH_DISABLE_PROVIDER_AUTO_UPDATE=1 to use the embedded providers bundled at the time of this Crush release. You can also update providers manually. For more info see crush update-providers --help.\n\nCause: %w", catwalkURL, err) //nolint:staticcheck
			}
			providers.Append(items...)
		})

		wg.Go(func() {
			if customProvidersOnly {
				return
			}
			path := cachePathFor("hyper")
			cfgSnapshot := cfg
			var refresher func(context.Context) error
			if len(opts) > 0 {
				refresher = opts[0]
			}
			hyperSyncer.Init(realHyperClient{
				baseURL:      hyper.BaseURL(),
				resolveKey:   func() string { return resolveHyperAPIKey(cfgSnapshot) },
				refreshToken: refresher,
			}, path, autoupdate)

			// As above: keep whatever provider we were handed. The syncer
			// already falls back to the cached or embedded copy, so an
			// error here means "could not refresh", not "no Hyper". This
			// matters more than for other providers because Hyper's
			// endpoint and model list live in the catalog rather than in
			// the user's config: dropping it signs a logged-in user out.
			item, err := hyperSyncer.Get(ctx)
			if err != nil {
				hyperErr = fmt.Errorf("Crush was unable to fetch updated information from Hyper: %w", err) //nolint:staticcheck
			}
			hyperProvider = item
		})

		wg.Wait()

		if hyperProvider.ID != "" {
			providerList = append([]catwalk.Provider{hyperProvider}, slices.Collect(providers.Seq())...)
		} else {
			providerList = slices.Collect(providers.Seq())
		}
		providerErr = errors.Join(catwalkErr, hyperErr)
	})
	return providerList, providerErr
}

// UpdateProviderInList replaces a provider in the memoized provider list
// returned by Providers(). This is used after re-fetching a single
// provider (e.g. Hyper after OAuth) so that all callers of Providers()
// see the updated entry without needing to reset sync.Once.
func UpdateProviderInList(provider catwalk.Provider) {
	for i, p := range providerList {
		if p.ID == provider.ID {
			providerList[i] = provider
			return
		}
	}
	// Provider not found in list; prepend it.
	providerList = append([]catwalk.Provider{provider}, providerList...)
}

type cache[T any] struct {
	path string
}

func newCache[T any](path string) cache[T] {
	return cache[T]{path: path}
}

func (c cache[T]) Get() (T, string, error) {
	var v T
	data, err := os.ReadFile(c.path)
	if err != nil {
		return v, "", fmt.Errorf("failed to read provider cache file: %w", err)
	}

	if err := json.Unmarshal(data, &v); err != nil {
		return v, "", fmt.Errorf("failed to unmarshal provider data from cache: %w", err)
	}

	return v, etag.Of(data), nil
}

func (c cache[T]) Store(v T) error {
	slog.Info("Saving provider data to disk", "path", c.path)
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return fmt.Errorf("failed to create directory for provider cache: %w", err)
	}

	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("failed to marshal provider data: %w", err)
	}

	// Written through a temporary file and renamed into place. Several Crush
	// instances start independently and race to refresh this cache, and a
	// truncating write would let one of them read a half-written catalog and
	// silently fall back to the bundled copy.
	if err := atomicWriteFile(c.path, data, 0o644); err != nil {
		return fmt.Errorf("failed to write provider data to cache: %w", err)
	}
	return nil
}
