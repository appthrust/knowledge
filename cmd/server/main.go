package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/appthrust/appserver-go/appserver/platformenv"
)

func main() {
	listen := flag.String("listen", ":8080", "HTTP listen address")
	defaultStaticDir := os.Getenv("KNOWLEDGE_STATIC_DIR")
	if defaultStaticDir == "" {
		defaultStaticDir = "/srv/knowledge"
	}
	staticDir := flag.String("static-dir", defaultStaticDir, "static SPA directory")
	flag.Parse()
	if flag.NArg() != 0 {
		log.Fatal("usage: knowledge-server [--listen :8080] [--static-dir /srv/knowledge]")
	}
	platform, err := platformenv.FromEnv()
	if err != nil {
		log.Fatal(err)
	}
	if err := platformenv.ValidateIdentityListen(*listen, platform.OIDCIssuer); err != nil {
		log.Fatal(err)
	}
	upstreamURL, projectID := platform.APIURL, platform.ProjectID
	// Validate provider configuration before opening the database.
	if _, err := runtimeOptions(projectID); err != nil {
		log.Fatal(err)
	}
	knowledge, closeKnowledge, err := knowledgeHandler(upstreamURL, projectID)
	if err != nil {
		log.Fatal(err)
	}
	defer closeKnowledge()
	handler, err := newHandler(*staticDir, upstreamURL, projectID, knowledge, knowledge.Ready)
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Addr: *listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()
	log.Printf("knowledge-server listening on %s", *listen)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
