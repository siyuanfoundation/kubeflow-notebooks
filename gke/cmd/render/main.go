package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/kubeflow/notebooks/gke/internal/deploy"
)

func main() {
	configPath := flag.String("config", "", "Path to non-secret JSON deployment configuration")
	stage := flag.String("stage", "", "namespaces, isolation, applications, or edge")
	root := flag.String("repo-root", "..", "Repository root; default assumes execution from gke/")
	flag.Parse()
	file, err := os.Open(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()
	config, err := deploy.ReadConfig(file)
	if err != nil {
		log.Fatal(err)
	}
	absoluteRoot, err := filepath.Abs(*root)
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	resources, err := deploy.Render(ctx, absoluteRoot, *stage, config, deploy.Kustomize)
	if err != nil {
		log.Fatal(err)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(map[string]any{"apiVersion": "v1", "kind": "List", "items": resources}); err != nil {
		log.Fatal(err)
	}
}
