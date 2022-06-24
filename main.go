package main

import (
	"context"
	"fmt"
	"github.com/filecoin-project/go-jsonrpc"
	"github.com/gorilla/mux"
	"github.com/urfave/cli/v2"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	app := &cli.App{
		Name:     "lotus-cpr",
		HelpName: "lotus-cpr",
		Usage:    "A caching proxy for Lotus filecoin nodes.",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "api",
				Usage:   "Multiaddress of Lotus node.",
				EnvVars: []string{"LOTUS_CPR_API"},
				Value:   "127.0.0.1:2345",
			},
			&cli.StringFlag{
				Name:     "api-token",
				Usage:    "Read only API token for Lotus node.",
				EnvVars:  []string{"LOTUS_CPR_API_TOKEN"},
				Required: true,
			},
			&cli.StringFlag{
				Name:    "listen",
				Usage:   "Address to start the jsonrpc server on.",
				EnvVars: []string{"LOTUS_CPR_LISTEN"},
				Value:   ":33111",
			},
		},
		Action:          run,
		HideHelpCommand: true,
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Println(err.Error())
		os.Exit(1)
	}
}

func run(cctx *cli.Context) error {
	ctx, cancel := context.WithCancel(cctx.Context)
	defer cancel()

	rpcAPI, err := NewProxiedRpcAPI(cctx.String("api-token"), cctx.String("api"))

	if err != nil {
		return fmt.Errorf("failed to create api client: %w", err)
	}
	defer rpcAPI.closer()

	rpcServer := jsonrpc.NewServer()
	rpcServer.Register("Filecoin", rpcAPI.minerAPI)

	// Set up a signal handler to cancel the context
	go func() {
		interrupt := make(chan os.Signal, 1)
		signal.Notify(interrupt, syscall.SIGTERM, syscall.SIGINT)
		select {
		case <-interrupt:
			cancel()
		case <-ctx.Done():
		}
	}()

	address := cctx.String("listen")
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("failed to listen on %q: %w", cctx.String("listen"), err)
	}

	mux := mux.NewRouter()

	mux.Use(ValidateToken)
	mux.Handle("/rpc/v0", rpcServer)
	mux.Handle("/rpc/v1", rpcServer)
	mux.PathPrefix("/").Handler(http.DefaultServeMux)

	srv := &http.Server{
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		if err := srv.Shutdown(context.Background()); err != nil {
			log.Println(err, "failed to shut down RPC server")
		}
	}()

	log.Println("Starting RPC server", "addr", cctx.String("listen"))
	return srv.Serve(listener)
}
