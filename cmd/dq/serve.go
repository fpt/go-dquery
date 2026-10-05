package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/fpt/go-dquery"
	"github.com/fpt/go-dquery/frontend/graphql"
)

// newMux exposes GraphQL at /graphql and the schema SDL at /graphql/schema.
func newMux(db *dquery.DB) (*http.ServeMux, error) {
	gs, err := graphql.NewSchema(db.Catalog)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/graphql", gs.Handler(db.Executor, db.Options))
	mux.Handle("GET /graphql/schema", gs.SDLHandler())
	return mux, nil
}

// serve runs the GraphQL server until SIGINT.
func serve(addr string, db *dquery.DB, log io.Writer) error {
	mux, err := newMux(db)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	fmt.Fprintf(log, "dq: serving GraphQL on http://%s/graphql (schema at /graphql/schema)\n", addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
