package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/luluz66/review_bot/app"
)

var (
	appID          = flag.Int64("github.app.id", -1, "GitHub app ID.")
	privateKeyPath = flag.String("github.app.private_key_path", "", "A Path to GitHub app private key.")
	webHookSecret  = flag.String("github.app.webhook_secret", "", "webhook secret")
	bbAPIKey       = flag.String("bb.api.key", "", "bb API Key")
	port           = flag.Int64("github.app.port", 3000, "port")
)

func main() {
	flag.Parse()
	if appID == nil || *appID == -1 {
		log.Fatal("require --github.app.id")
	}
	if privateKeyPath == nil || *privateKeyPath == "" {
		log.Fatal("require --github.app.private_key_path")
	}
	if webHookSecret == nil || *webHookSecret == "" {
		log.Fatal("require --github.app.webhook_secret")
	}
	ghApp, err := app.NewGithubApp(*appID, *privateKeyPath, *webHookSecret, *bbAPIKey)

	if err != nil {
		log.Fatalf("failed to create github app: %s", err)
	}

	addr := fmt.Sprintf("0.0.0.0:%d", *port)
	log.Printf("Listening on http://%s", addr)
	mux := http.NewServeMux()
	handle(mux, "/event_handler", ghApp.HandleWebhook)
	http.ListenAndServe(addr, mux)
}

func handle(mux *http.ServeMux, pattern string, handleFunc http.HandlerFunc) {
	mux.HandleFunc(pattern, func(w http.ResponseWriter, req *http.Request) {
		log.Printf("%s %s", req.Method, req.URL)
		handleFunc(w, req)
	})
	if !strings.HasSuffix(pattern, "/") {
		handle(mux, pattern+"/", handleFunc)
	}
}
