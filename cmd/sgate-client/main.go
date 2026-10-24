package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"sgate/internal/aclservice"
	"sgate/internal/app"
)

func main() {
	options, err := parseClientOptions()
	if err != nil {
		log.Fatal(err)
	}
	if aclservice.Enabled() {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := app.RunACL(ctx, "client", flag.CommandLine, os.Args[1:], options.adminConfig, nil); err != nil {
			log.Fatal(err)
		}
		return
	}
	if options.adminConfig != "" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := app.Run(ctx, "client", flag.CommandLine, os.Args[1:], options.adminConfig); err != nil {
			log.Fatal(err)
		}
		return
	}
	client, err := newStandaloneClient(options)
	if err != nil {
		log.Fatal(err)
	}
	listener, err := net.Listen("tcp", options.listen)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("sgate client listening on %s", listener.Addr())
	if err = client.Run(ctx, listener); err != nil {
		log.Fatal(err)
	}
}
