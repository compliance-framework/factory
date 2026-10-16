package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/auth"
	"github.com/compliance-framework/factory/internal/catalog"
	"github.com/compliance-framework/factory/internal/catalog/gitsource"
	"github.com/compliance-framework/factory/internal/clock"
	"github.com/compliance-framework/factory/internal/controllers"
	"github.com/compliance-framework/factory/internal/provider"
	"github.com/compliance-framework/factory/internal/provider/fake"
	"github.com/compliance-framework/factory/internal/scheduler"
	"github.com/compliance-framework/factory/internal/server"
	"github.com/compliance-framework/factory/internal/store"
	"github.com/compliance-framework/factory/internal/store/memory"
	"github.com/compliance-framework/factory/internal/webui"
)

// githubHTTPTimeout bounds each call to GitHub's OAuth and user endpoints.
const githubHTTPTimeout = 30 * time.Second

// serveDeps are runServe's injectable dependencies; the zero value is production.
type serveDeps struct {
	// Getenv reads the catalog secretRefs (FACTORY_SECRET_<NAME>_<KEY>). Default: none set.
	Getenv func(string) string
	// Clock. Default clock.Real.
	Clock clock.Clock
	// Fake is the fake adapter --dev-fake-providers registers (a new one when nil), so a test
	// can seed it.
	Fake *fake.Adapter
	// HTTPClient calls GitHub (OAuth, GET /user). Default a client with a 30 s timeout.
	HTTPClient *http.Client
	// Listening, when set, gets the bound public and ops addresses once both listen.
	Listening func(public, ops net.Addr)
	// Ready, when set, is called once startup is done (rebuild finished, controllers up).
	Ready func()
}

// adapters are the provider adapters `factory serve` registers.
func adapters(cfg serveConfig, d serveDeps, groups provider.GroupLookup) []provider.Adapter {
	var out []provider.Adapter
	// GH-S1-T04 registers the GitHub adapter here (one line), unless --dev-fake-providers
	// stands in for it.
	if cfg.DevFakeProviders {
		a := d.Fake
		if a == nil {
			a = fake.NewAdapter(fake.Options{Clock: d.Clock, Groups: groups})
		}
		out = append(out, a)
	}
	return out
}

// envSecrets resolves a catalog secretRef from FACTORY_SECRET_<NAME>_<KEY>.
type envSecrets func(string) string

// secretEnv is the variable of a secretRef: FACTORY_SECRET_ + NAME + _ + KEY, upper-cased, every
// character but A-Z and 0-9 replaced by '_'.
func secretEnv(ref v1alpha1.SecretRef) string {
	norm := func(s string) string {
		return strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z':
				return r - 'a' + 'A'
			case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
				return r
			}
			return '_'
		}, s)
	}
	return envSecretPrefix + norm(ref.Name) + "_" + norm(ref.Key)
}

// Resolve implements provider.SecretResolver. The error names the variable, never a value.
func (e envSecrets) Resolve(_ context.Context, ref v1alpha1.SecretRef) ([]byte, error) {
	name := secretEnv(ref)
	v := e(name)
	if v == "" {
		return nil, fmt.Errorf("secret %s/%s: %s is not set", ref.Name, ref.Key, name)
	}
	return []byte(v), nil
}

// runGroup runs the long-lived components and reports the first one that fails.
type runGroup struct {
	wg   sync.WaitGroup
	errc chan error
}

func newRunGroup() *runGroup { return &runGroup{errc: make(chan error, 1)} }

// Go runs f(ctx); an error while ctx is live is reported on errc (the first one only).
func (g *runGroup) Go(ctx context.Context, name string, f func(context.Context) error) {
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		if err := f(ctx); err != nil && ctx.Err() == nil {
			select {
			case g.errc <- fmt.Errorf("%s: %w", name, err):
			default:
			}
		}
	}()
}

// runServe runs the dispatcher until ctx is done, then shuts it down gracefully. Startup order
// (controllers.md §Ordering and startup): store → catalog-sync (wait for the first good catalog)
// → providers (connect, recover) → listeners (webhooks are stored from now on) → work-item
// rebuild → rule, debounce, work-item, run controllers and the scheduler → ready.
func runServe(ctx context.Context, cfg serveConfig, d serveDeps, log *slog.Logger) error {
	if d.Getenv == nil {
		d.Getenv = func(string) string { return "" }
	}
	if d.Clock == nil {
		d.Clock = clock.Real{}
	}
	if d.HTTPClient == nil {
		d.HTTPClient = &http.Client{Timeout: githubHTTPTimeout}
	}
	clk := d.Clock
	startedAt := clk.Now()
	secrets := envSecrets(d.Getenv)

	log.Info("factory serve starting",
		"addr", cfg.Addr, "opsAddr", cfg.OpsAddr, "publicURL", cfg.PublicURL,
		"catalog", catalogSource(cfg).String(), "retention", cfg.Retention.String(),
		"devFakeProviders", cfg.DevFakeProviders)

	// Components run under runCtx, which is cancelled only once the listeners have drained,
	// so in-flight completes still reach the run controller.
	runCtx, cancelRun := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRun()
	group := newRunGroup()
	defer group.wg.Wait()
	defer cancelRun()

	// Store and retention.
	st := memory.New(store.Options{Clock: clk, Retention: cfg.Retention, Logger: log})
	group.Go(runCtx, "sweeper", func(ctx context.Context) error {
		store.RunSweeper(ctx, st, 0, log.With("component", "sweeper"))
		return nil
	})

	// Catalog.
	cs := controllers.NewCatalogSync(catalogSource(cfg), controllers.CatalogSyncOptions{
		Interval: cfg.CatalogInterval, Clock: clk, Logger: log,
	})
	group.Go(runCtx, "catalog-sync", cs.Run)
	if err := waitCatalog(ctx, cs, cfg.CatalogTimeout); err != nil {
		return err
	}
	cat := cs.Current()
	log.Info("catalog loaded", "sha", cat.SHA, "providers", len(cat.Providers), "ruleSets", len(cat.RuleSets))

	// Providers.
	groups := func(name string) (v1alpha1.GroupSpec, bool) {
		c := cs.Current()
		if c == nil {
			return v1alpha1.GroupSpec{}, false
		}
		g, ok := c.Groups[name]
		if !ok {
			return v1alpha1.GroupSpec{}, false
		}
		return g.Spec, true
	}
	reg, err := provider.NewRegistry(adapters(cfg, d, groups)...)
	if err != nil {
		return fmt.Errorf("provider registry: %w", err)
	}
	pc, err := controllers.NewProviderController(st, cs, reg, controllers.ProviderOptions{Clock: clk, Logger: log, Secrets: secrets})
	if err != nil {
		return err
	}
	if err := pc.Connect(ctx); err != nil {
		hint := ""
		if errors.Is(err, provider.ErrUnknownType) && !cfg.DevFakeProviders {
			hint = " (no GitHub adapter is built in yet: use --dev-fake-providers for development)"
		}
		return fmt.Errorf("connect providers: %w%s", err, hint)
	}
	for _, name := range pc.Names() {
		log.Info("provider connected", "provider", name, "type", cat.Providers[name].Spec.Type,
			"webhook", cfg.PublicURL+"/hooks/"+name)
	}
	group.Go(runCtx, "provider", pc.Run)
	if err := pc.Recover(ctx, startedAt); err != nil {
		log.Warn("recovering deliveries failed; continuing", "err", err)
	}
	warnPoolSecrets(ctx, cat, secrets, log)

	// Controllers, scheduler, auth and the API server (constructed; started below).
	authProvider, apiBase, err := githubAuth(cfg, cat)
	if err != nil {
		return err
	}
	factory := controllers.NewFactoryProvider(st, clk)
	cache := controllers.NewObservationCache(pc, clk, 0)
	var skills controllers.SkillResolver = controllers.KeepSkills{}
	if cfg.SkillsBase != "" {
		skills = &controllers.GitSkills{Base: cfg.SkillsBase}
	}
	rule, err := controllers.NewRuleController(st, cs, pc, controllers.RuleOptions{
		Clock: clk, Logger: log, Cache: cache, Skills: skills, UIBaseURL: cfg.PublicURL,
	})
	if err != nil {
		return err
	}
	dbn, err := controllers.NewDebounceController(rule)
	if err != nil {
		return err
	}
	wic, err := controllers.NewWorkItemController(st, cs, pc, factory, controllers.WorkItemOptions{
		Clock: clk, Logger: log, Cache: cache, Retention: cfg.Retention,
	})
	if err != nil {
		return err
	}
	runs, err := controllers.NewRunController(st, cs, factory, controllers.RunOptions{Clock: clk, Logger: log})
	if err != nil {
		return err
	}
	sched, err := scheduler.New(scheduler.Deps{
		Store: st, Catalog: cs, Conns: pc, Runs: runs, Secrets: secrets,
		Owners: auth.GitHubOwners{Provider: authProvider, APIBase: apiBase, HTTPClient: d.HTTPClient},
	}, scheduler.Options{Clock: clk, Logger: log})
	if err != nil {
		return err
	}
	authn, err := auth.New(auth.Config{
		CLIToken: cfg.CLIToken,
		OAuth: auth.OAuthConfig{
			ClientID: cfg.OAuthClientID, ClientSecret: cfg.OAuthClientSecret,
			AuthorizeURL: cfg.GitHubURL + "/login/oauth/authorize",
			TokenURL:     cfg.GitHubURL + "/login/oauth/access_token",
			APIBase:      apiBase,
			RedirectURL:  cfg.PublicURL + server.BasePath + "/auth/callback",
		},
		Provider:   authProvider,
		SessionKey: []byte(cfg.SessionKey),
		HTTPClient: d.HTTPClient,
		Clock:      clk,
		Logger:     log,
	}, auth.Deps{Store: st, Catalog: cs, Conns: pc, Agents: sched})
	if err != nil {
		return err
	}
	ready := make(chan struct{})
	repo, ref := "", ""
	if cfg.CatalogDir == "" {
		repo, ref = gitsource.RedactURL(cfg.CatalogRepo), cfg.CatalogRef
	}
	srv, err := server.New(server.Deps{
		Store: st, Transcripts: st, Catalog: cs, CatalogStatus: cs, Resync: cs,
		Conns: pc, Sink: pc, Scheduler: sched, Factory: factory,
		KillSwitches: controllers.StoreKillSwitches{Store: st}, Evaluator: rule, Auth: authn,
	}, server.Options{
		PublicURL: cfg.PublicURL, CatalogRepo: repo, CatalogRef: ref, Ready: ready,
		Controllers: []*controllers.Controller{
			cs.Controller(), pc.Controller(), rule.Controller(), dbn.Controller(),
			wic.Controller(), runs.Controller(), sched.Controller(),
		},
		Clock: clk, Logger: log,
	})
	if err != nil {
		return err
	}
	ui := echo.WrapHandler(webui.Handler())
	srv.Echo().GET("/*", ui)
	srv.Echo().HEAD("/*", ui)

	// Listeners.
	pubL, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on --addr: %w", err)
	}
	opsL, err := net.Listen("tcp", cfg.OpsAddr)
	if err != nil {
		_ = pubL.Close()
		return fmt.Errorf("listen on --ops-addr: %w", err)
	}
	errLog := slog.NewLogLogger(log.With("component", "http").Handler(), slog.LevelWarn)
	public := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute, ErrorLog: errLog}
	ops := &http.Server{Handler: srv.OpsHandler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute, ErrorLog: errLog}
	serveErr := make(chan error, 2)
	var serving sync.WaitGroup
	for _, l := range []struct {
		s *http.Server
		l net.Listener
	}{{public, pubL}, {ops, opsL}} {
		serving.Add(1)
		go func() {
			defer serving.Done()
			if err := l.s.Serve(l.l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serveErr <- err
			}
		}()
	}
	log.Info("listening", "addr", pubL.Addr().String(), "opsAddr", opsL.Addr().String())
	if d.Listening != nil {
		d.Listening(pubL.Addr(), opsL.Addr())
	}

	shutdown := func(reason string) error {
		log.Info("shutting down", "reason", reason)
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
		defer cancel()
		var errs []error
		// SSE streams and lease long polls end first, or Shutdown would wait for them.
		if err := srv.Shutdown(sctx); err != nil {
			errs = append(errs, fmt.Errorf("end streams: %w", err))
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		for _, s := range []*http.Server{public, ops} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := s.Shutdown(sctx); err != nil {
					_ = s.Close()
					mu.Lock()
					errs = append(errs, fmt.Errorf("drain requests: %w", err))
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		serving.Wait()
		cancelRun()
		group.wg.Wait()
		if err := errors.Join(errs...); err != nil {
			log.Warn("shutdown incomplete", "err", err)
			return err
		}
		log.Info("shutdown complete")
		return nil
	}

	// Rebuild, then the controllers.
	if _, err := wic.Rebuild(ctx); err != nil {
		_ = shutdown("rebuild failed")
		return fmt.Errorf("work-item rebuild: %w", err)
	}
	group.Go(runCtx, "rule", rule.Run)
	group.Go(runCtx, "debounce", dbn.Run)
	group.Go(runCtx, "work-item", wic.Run)
	group.Go(runCtx, "run", runs.Run)
	group.Go(runCtx, "scheduler", sched.Run)
	close(ready)
	log.Info("ready")
	if d.Ready != nil {
		d.Ready()
	}

	select {
	case <-ctx.Done():
		return shutdown("signal")
	case err := <-serveErr:
		_ = shutdown("listener failed")
		return fmt.Errorf("serve: %w", err)
	case err := <-group.errc:
		_ = shutdown("component failed")
		return err
	}
}

// catalogSource is the catalog-sync source cfg names.
func catalogSource(cfg serveConfig) controllers.CatalogSource {
	if cfg.CatalogDir != "" {
		return controllers.NewDirSource(cfg.CatalogDir, catalog.Options{})
	}
	return controllers.GitSource{Repo: cfg.CatalogRepo, Ref: cfg.CatalogRef, Path: cfg.CatalogPath}
}

// waitCatalog waits for catalog-sync's first good catalog, at most timeout.
func waitCatalog(ctx context.Context, cs *controllers.CatalogSync, timeout time.Duration) error {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-cs.Ready():
		return nil
	case <-ctx.Done():
		return fmt.Errorf("waiting for the catalog: %w", ctx.Err())
	case <-t.C:
	}
	st := cs.Status()
	var why []string
	if st.SourceError != "" {
		why = append(why, "source: "+st.SourceError)
	}
	for i, e := range st.Errors {
		if i == 3 {
			why = append(why, fmt.Sprintf("… %d more", len(st.Errors)+st.ErrorsOmitted-3))
			break
		}
		why = append(why, e.Error())
	}
	if len(why) == 0 {
		why = append(why, "no valid catalog loaded yet")
	}
	return fmt.Errorf("no valid catalog from %s after %s: %s", st.Source, timeout, strings.Join(why, "; "))
}

// githubAuth returns the catalog Provider the GitHub logins belong to and the API base for
// GET /user: --auth-provider (else the catalog's only github Provider), and --github-api-url
// (else that Provider's spec.github.apiURL, else api.github.com).
func githubAuth(cfg serveConfig, cat *catalog.Catalog) (string, string, error) {
	name := cfg.AuthProvider
	if name == "" {
		var gh []string
		for n, p := range cat.Providers {
			if p.Spec.Type == v1alpha1.ProviderTypeGitHub {
				gh = append(gh, n)
			}
		}
		slices.Sort(gh)
		if len(gh) != 1 {
			return "", "", fmt.Errorf("the catalog has %d github Providers %v: set --auth-provider", len(gh), gh)
		}
		name = gh[0]
	}
	p, ok := cat.Providers[name]
	if !ok {
		return "", "", fmt.Errorf("--auth-provider %q is not a catalog Provider", name)
	}
	if p.Spec.Type != v1alpha1.ProviderTypeGitHub {
		return "", "", fmt.Errorf("--auth-provider %q is of type %q, not github", name, p.Spec.Type)
	}
	api := cfg.GitHubAPIURL
	if api == "" && p.Spec.GitHub != nil {
		api = strings.TrimRight(p.Spec.GitHub.APIURL, "/")
	}
	if api == "" {
		api = auth.DefaultAPIBase
	}
	return name, api, nil
}

// warnPoolSecrets warns about registered Pools whose join token isn't set: no agent can join
// them.
func warnPoolSecrets(ctx context.Context, cat *catalog.Catalog, secrets provider.SecretResolver, log *slog.Logger) {
	for _, name := range sortedKeys(cat.Pools) {
		p := cat.Pools[name]
		if p.Spec.Admission == nil {
			continue
		}
		if _, err := secrets.Resolve(ctx, p.Spec.Admission.TokenRef); err != nil {
			log.Warn("pool join token not set: agents can't register", "pool", name, "env", secretEnv(p.Spec.Admission.TokenRef))
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
