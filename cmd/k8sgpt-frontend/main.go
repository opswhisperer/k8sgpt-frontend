package main

import (
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	flag "github.com/spf13/pflag"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	resultNS := flag.String("result-namespace", "k8sgpt-operator-system", "namespace to list results from")
	kubeconfig := flag.String("kubeconfig", "", "path to kubeconfig (empty = in-cluster)")
	appriseURL := flag.String("apprise-url", "", "Apprise API endpoint URL (empty = disabled)")
	pollInterval := flag.Int("poll-interval", 60, "polling interval in seconds")
	flag.Parse()

	// env-var fallback: override flag default when the flag was not set
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

	envOverrideString("addr", "ADDR")
	envOverrideString("result-namespace", "RESULT_NAMESPACE")
	envOverrideString("kubeconfig", "KUBECONFIG")
	envOverrideString("apprise-url", "APPRISE_URL")
	envOverrideInt("poll-interval", "POLL_INTERVAL")

	// Build k8s config: try in-cluster first, fall back to kubeconfig file.
	cfg, err := rest.InClusterConfig()
	if err != nil {
		cfg, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
		if err != nil {
			log.Fatalf("cannot build k8s config: %v", err)
		}
	}

	clients, err := buildClients(cfg)
	if err != nil {
		log.Fatalf("cannot create k8s clients: %v", err)
	}

	// Start background poller only when an Apprise URL is configured.
	if *appriseURL != "" {
		interval := time.Duration(*pollInterval) * time.Second
		log.Printf("starting poller: apprise-url=%s poll-interval=%s", *appriseURL, interval)
		go runPoller(clients, *resultNS, *appriseURL, interval)
	}

	mux := http.NewServeMux()
	registerHandlers(mux, clients, *resultNS)

	log.Printf("listening on %s", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
