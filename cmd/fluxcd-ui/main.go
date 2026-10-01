// Command fluxcd-ui serves a web UI listing Flux objects and their
// reconciliation state.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/mycroft/fluxcd-ui/internal/authz"
	"github.com/mycroft/fluxcd-ui/internal/store"
	"github.com/mycroft/fluxcd-ui/internal/web"
)

var version = "dev"

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn or error")
	logFormat := flag.String("log-format", "text", "log format: text or json")
	enableActions := flag.Bool("enable-actions", false, "allow suspending, resuming and reconciling objects (requires patch RBAC)")
	userHeader := flag.String("user-header", "", "request header carrying the user name set by an authenticating proxy, e.g. X-authentik-username; when set, actions require it")
	groupsHeader := flag.String("groups-header", "", "request header carrying the user's groups set by the proxy, e.g. X-authentik-groups")
	groupsSeparator := flag.String("groups-separator", ",", `separator of the groups header ("|" for authentik)`)
	authorization := flag.String("authorization", "none", `who may act: "none" (every user) or "rbac" (Kubernetes RBAC, via SubjectAccessReviews)`)
	subjectPrefix := flag.String("subject-prefix", "fluxcd-ui:", "prefix added to user and group names before checking RBAC")
	fluxNamespace := flag.String("flux-namespace", "flux-system", "namespace of the Flux controllers, whose logs the drawer shows")
	controllerLogs := flag.Bool("controller-logs", true, "offer the Flux controllers' logs about an object in its drawer (requires reading pods and pods/log in --flux-namespace)")
	diffEnabled := flag.Bool("diff", true, "offer to diff a Kustomization's source with the cluster (requires reaching source-controller's artifacts, and read access to the managed objects)")
	managedStatus := flag.Bool("managed-objects-status", true, "show the health of the objects a Kustomization or HelmRelease manages (requires read access to them, e.g. the built-in view ClusterRole)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse() // also parses --kubeconfig, registered by controller-runtime

	if *showVersion {
		fmt.Println(version)
		return
	}

	log, err := newLogger(*logLevel, *logFormat)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	opts := web.Options{
		Version:         version,
		Actions:         *enableActions,
		UserHeader:      *userHeader,
		GroupsHeader:    *groupsHeader,
		GroupsSeparator: *groupsSeparator,
		Logs:            *controllerLogs,
		ManagedStatus:   *managedStatus,
		Diff:            *diffEnabled,
	}
	if err := checkAuthorization(*authorization, *userHeader, *subjectPrefix); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := run(*addr, opts, *authorization, *subjectPrefix, *fluxNamespace, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func checkAuthorization(mode, userHeader, prefix string) error {
	switch mode {
	case "none":
		return nil
	case "rbac":
		if userHeader == "" {
			return errors.New("--authorization=rbac requires --user-header")
		}
		if err := authz.ValidatePrefix(prefix); err != nil {
			return fmt.Errorf("--subject-prefix: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("invalid --authorization %q: want none or rbac", mode)
	}
}

func run(addr string, opts web.Options, authorization, subjectPrefix, fluxNamespace string, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.GetConfig()
	if err != nil {
		return fmt.Errorf("loading kubeconfig: %w", err)
	}
	cfg.UserAgent = "fluxcd-ui/" + version

	broker := store.NewBroker(300 * time.Millisecond)
	st, err := store.NewForCluster(cfg, broker, fluxNamespace, log)
	if err != nil {
		return err
	}
	if authorization == "rbac" {
		c, err := client.New(cfg, client.Options{}) // client-go's scheme has SubjectAccessReview
		if err != nil {
			return fmt.Errorf("creating client: %w", err)
		}
		if opts.Authorizer, err = authz.NewRBAC(c, subjectPrefix, 30*time.Second); err != nil {
			return err
		}
	}
	srv, err := web.New(st, broker, opts, log)
	if err != nil {
		return err
	}

	errs := make(chan error, 2)
	go func() {
		if err := st.Start(ctx); err != nil {
			errs <- fmt.Errorf("running cache: %w", err)
		}
	}()

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: /events keeps connections open.
	}
	go func() {
		log.Info("listening", "addr", addr, "version", version, "actions", opts.Actions, "authorization", authorization, "userHeader", opts.UserHeader, "groupsHeader", opts.GroupsHeader)
		if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-errs:
		return err
	}

	broker.Close() // ends SSE streams so Shutdown does not wait on them
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}

func newLogger(level, format string) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid --log-level %q", level)
	}
	opts := &slog.HandlerOptions{Level: lvl}

	var h slog.Handler
	switch format {
	case "text":
		h = slog.NewTextHandler(os.Stderr, opts)
	case "json":
		h = slog.NewJSONHandler(os.Stderr, opts)
	default:
		return nil, fmt.Errorf("invalid --log-format %q", format)
	}

	// Route controller-runtime and client-go logs through slog too.
	ctrllog.SetLogger(logr.FromSlogHandler(h))
	klog.SetLogger(logr.FromSlogHandler(h))
	return slog.New(h), nil
}
