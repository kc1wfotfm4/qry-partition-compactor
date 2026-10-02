// Command qry-partition-compactor serves the HTTP API described in README.md.
package main

import (
	"log"
	"os"

	"github.com/kc1wfotfm4/qry-partition-compactor/internal/api"
	"github.com/kc1wfotfm4/qry-partition-compactor/internal/store"
)

func main() {
	address := os.Getenv("ADDR")
	if address == "" {
		address = "127.0.0.1:8080"
	}
	databasePath := os.Getenv("DB_PATH")
	if databasePath == "" {
		databasePath = "qry-partition-compactor.db"
	}

	st, err := store.Open(databasePath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	if err := api.NewRouter(st).Run(address); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
