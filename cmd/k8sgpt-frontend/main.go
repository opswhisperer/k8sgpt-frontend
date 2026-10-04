package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	flag "github.com/spf13/pflag"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

func main() {
	addr := flag.String("addr", ":8080", "listen address (env: ADDR)")
	resultNS := flag.String("result-namespace", "k8sgpt-operator-system", "namespace holding the K8sGPT and Result resources (env: RESULT_NAMESPACE)")
	kubeconfig := flag.String("kubeconfig", "", "path to kubeconfig; empty = in-cluster (env: KUBECONFIG)")
	dataDir := flag.String("data-dir", "data", "directory for ignores, rules and notification state; empty = memory only (env: DATA_DIR)")
	configPath := flag.String("config", "", "optional YAML config file with hide rules (env: CONFIG)")
	readOnly := flag.Bool("read-only", false, "disable hiding, rule changes and test notifications from the UI (env: READ_ONLY)")
	appriseURL := flag.String("apprise-url", "", "Apprise API /notify/ endpoint URL; empty = notifications disabled (env: APPRISE_URL)")
	appriseFormat := flag.String("apprise-format", "html", "notification body format: html, markdown or text (env: APPRISE_FORMAT)")
	appriseTag := flag.String("apprise-tag", "", "Apprise tag to notify; empty = all (env: APPRISE_TAG)")
	uiURL := flag.String("ui-url", "", "external URL of this UI, used for links in notifications (env: UI_URL)")
	pollInterval := flag.Int("poll-interval", 60, "how often to poll, in seconds (env: POLL_INTERVAL)")
	notifyDelay := flag.Int("notify-delay", 300, "seconds an issue must persist before it is notified; 0 = immediately (env: NOTIFY_DELAY)")
	notifyMax := flag.Int("notify-max-items", 15, "most issues listed in one notification (env: NOTIFY_MAX_ITEMS)")
	notifyResolved := flag.Bool("notify-resolved", false, "also notify when notified issues clear (env: NOTIFY_RESOLVED)")
	healthWindow := flag.Int("health-window", 2700, "seconds an analysis failure counts as current (env: HEALTH_WINDOW)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		_, _ = os.Stdout.WriteString(version + "\n")
		return
	}

	// env-var fallback: override the flag default when the flag was not set
	// explicitly on the command line and the env var is non-empty.
	// Precedence: --flag > ENV_VAR > compiled default.
	envOverrideString := func(flagName, envName string) {
		if v := os.Getenv(envName); v != "" && !flag.CommandLine.Changed(flagName) {
			_ = flag.CommandLine.Set(flagName, v)
		}
	}
	envOverrideInt := func(flagName, envName string) {
		if v := os.Getenv(envName); v != "" && !flag.CommandLine.Changed(flagName) {
			if _, err := strconv.Atoi(v); err == nil {
				_ = flag.CommandLine.Set(flagName, v)
			}
		}
	}
	envOverrideBool := func(flagName, envName string) {
		if v := os.Getenv(envName); v != "" && !flag.CommandLine.Changed(flagName) {
			if _, err := strconv.ParseBool(v); err == nil {
				_ = flag.CommandLine.Set(flagName, v)
			}
		}
	}

	envOverrideString("addr", "ADDR")
	envOverrideString("result-namespace", "RESULT_NAMESPACE")
	envOverrideString("kubeconfig", "KUBECONFIG")
	envOverrideString("data-dir", "DATA_DIR")
	envOverrideString("config", "CONFIG")
	envOverrideBool("read-only", "READ_ONLY")
	envOverrideString("apprise-url", "APPRISE_URL")
	envOverrideString("apprise-format", "APPRISE_FORMAT")
	envOverrideString("apprise-tag", "APPRISE_TAG")
	envOverrideString("ui-url", "UI_URL")
	envOverrideInt("poll-interval", "POLL_INTERVAL")
	envOverrideInt("notify-delay", "NOTIFY_DELAY")
	envOverrideInt("notify-max-items", "NOTIFY_MAX_ITEMS")
	envOverrideBool("notify-resolved", "NOTIFY_RESOLVED")
	envOverrideInt("health-window", "HEALTH_WINDOW")

	if *pollInterval < 10 {
		*pollInterval = 10
	}
	switch *appriseFormat {
	case "html", "markdown", "text":
	default:
		log.Fatalf("--apprise-format must be html, markdown or text, not %q", *appriseFormat)
	}

	// Build k8s config: try in-cluster first, fall back to kubeconfig file.
	cfg, err := rest.InClusterConfig()
	if err != nil {
		cfg, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
		if err != nil {
			log.Fatalf("cannot build k8s config: %v", err)
		}
	}
	kube, err := newKube(cfg, *resultNS)
	if err != nil {
		log.Fatalf("cannot create k8s clients: %v", err)
	}

	conf, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	store := openStore(*dataDir)
	notifier := &Notifier{URL: *appriseURL, UIURL: *uiURL, Format: *appriseFormat, Tag: *appriseTag, MaxItems: *notifyMax}
	app := newApp(kube, store, notifier, conf.Rules, Options{
		Namespace:      *resultNS,
		PollInterval:   time.Duration(*pollInterval) * time.Second,
		NotifyDelay:    time.Duration(*notifyDelay) * time.Second,
		HealthWindow:   time.Duration(*healthWindow) * time.Second,
		NotifyResolved: *notifyResolved,
		ReadOnly:       *readOnly,
	})

	log.Printf("k8sgpt-frontend %s: namespace=%s data-dir=%q config-rules=%d read-only=%t notifications=%t format=%s poll=%ds delay=%ds",
		version, *resultNS, *dataDir, len(conf.Rules), *readOnly, notifier.Enabled(), *appriseFormat, *pollInterval, *notifyDelay)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go app.Run(ctx)

	mux := http.NewServeMux()
	registerHandlers(mux, app)
	srv := &http.Server{Addr: *addr, Handler: logRequests(mux), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	log.Printf("listening on %s", *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}

// logRequests logs mutating API calls; reads and probes stay quiet.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			log.Printf("%s %s", r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}
