package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/stuttgart-things/machinery-order-service/api/v1alpha1"
	"github.com/stuttgart-things/machinery-order-service/internal/api"
	"github.com/stuttgart-things/machinery-order-service/internal/auth"
	"github.com/stuttgart-things/machinery-order-service/internal/controller"
	"github.com/stuttgart-things/machinery-order-service/internal/delivery"
	"github.com/stuttgart-things/machinery-order-service/internal/ghauth"
	"github.com/stuttgart-things/machinery-order-service/internal/machinery"
	"github.com/stuttgart-things/machinery-order-service/internal/render"
	"github.com/stuttgart-things/machinery-order-service/internal/web"
)

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envBool(name string) bool {
	switch strings.ToLower(os.Getenv(name)) {
	case "1", "true", "yes":
		return true
	}
	return false
}

func envDuration(name string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s: want a positive duration like 30s, got %q", name, v)
	}
	return d, nil
}

func setupLogging() {
	level := slog.LevelInfo
	if envBool("DEBUG") {
		level = slog.LevelDebug
	}
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler = slog.NewTextHandler(os.Stderr, opts)
	if os.Getenv("LOG_FORMAT") == "json" {
		h = slog.NewJSONHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(h))
	ctrl.SetLogger(logr.FromSlogHandler(h))
}

// namespace is where the Orders live: ORDER_NAMESPACE, else the pod's.
func namespace() string {
	if ns := os.Getenv("ORDER_NAMESPACE"); ns != "" {
		return ns
	}
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	if raw, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		return strings.TrimSpace(string(raw))
	}
	return "machinery-order-service"
}

// apiRunnable serves the API on every replica, leader or not.
type apiRunnable struct{ srv *http.Server }

func (a apiRunnable) NeedLeaderElection() bool { return false }

func (a apiRunnable) Start(ctx context.Context) error {
	errc := make(chan error, 1)
	go func() { errc <- a.srv.ListenAndServe() }()
	slog.Info("API listening", "addr", a.srv.Addr)
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return a.srv.Shutdown(sctx)
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func serve(ctx context.Context) error {
	setupLogging()
	ns := namespace()

	renderer, err := render.FromEnv()
	if err != nil {
		return err
	}
	src, apiURL, err := ghauth.FromEnv()
	if err != nil {
		return err
	}
	if src == nil {
		return fmt.Errorf("GitHub access is required: %s/%s/%s (the bot identity) or %s",
			ghauth.EnvAppID, ghauth.EnvAppInstallationID, ghauth.EnvAppPrivateKeyFile, ghauth.EnvToken)
	}
	git, err := delivery.New(apiURL, &http.Client{Timeout: 30 * time.Second, Transport: &ghauth.Transport{Source: src}})
	if err != nil {
		return err
	}
	authCfg, err := auth.LoadConfig()
	if err != nil {
		return err
	}
	authn, err := auth.New(authCfg)
	if err != nil {
		return err
	}
	if authn == nil {
		slog.Warn("orders are open: AUTH_OIDC_ISSUER is not set")
	}
	prPoll, err := envDuration("PR_POLL_INTERVAL", time.Minute)
	if err != nil {
		return err
	}
	livePoll, err := envDuration("LIVE_POLL_INTERVAL", 30*time.Second)
	if err != nil {
		return err
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return err
	}
	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("kubernetes: %w", err)
	}
	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: env("METRICS_ADDR", ":8081")},
		HealthProbeBindAddress:  env("PROBE_ADDR", ":8082"),
		LeaderElection:          envBool("LEADER_ELECT"),
		LeaderElectionID:        "machinery-order-service.order.machinery.stuttgart-things.com",
		LeaderElectionNamespace: ns,
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			&v1alpha1.Order{}: {Namespaces: map[string]cache.Config{ns: {}}},
		}},
	})
	if err != nil {
		return fmt.Errorf("manager: %w", err)
	}
	dialer := &machinery.GRPCDialer{}
	defer dialer.Close()
	if err := (&controller.OrderReconciler{
		Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(),
		Renderer: renderer, Git: git, Machinery: dialer, Namespace: ns,
		PRPoll: prPoll, LivePoll: livePoll,
	}).SetupWithManager(mgr); err != nil {
		return err
	}
	_ = mgr.AddHealthzCheck("ping", healthz.Ping)
	_ = mgr.AddReadyzCheck("ping", healthz.Ping)

	hub := api.NewHub()
	inf, err := mgr.GetCache().GetInformer(ctx, &v1alpha1.Order{})
	if err != nil {
		return err
	}
	if _, err := inf.AddEventHandler(hub); err != nil {
		return err
	}

	basePath := strings.TrimRight(os.Getenv("BASE_PATH"), "/")
	srv := &api.Server{
		Client: mgr.GetClient(), Namespace: ns, Auth: authn, Renderer: renderer, Hub: hub, BasePath: basePath,
		Ready: func() bool { return inf.HasSynced() },
	}
	srv.Extra = (&web.UI{API: srv, BasePath: basePath}).Register
	if err := mgr.Add(apiRunnable{srv: &http.Server{
		Addr: ":" + env("PORT", "8080"), Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second,
	}}); err != nil {
		return err
	}
	slog.Info("starting", "namespace", ns, "claimAPI", renderer.BaseURL, "github", apiURL, "basePath", basePath, "auth", authn != nil)
	return mgr.Start(ctx)
}
