package main

import (
	"flag"
	"log"
	"net"
	"net/http"
	"net/url"
	"time"

	"src.agwa.name/go-listener"
	_ "src.agwa.name/go-listener/tls"

	"src.agwa.name/sunglasses/proxy"
)

func parseURLFunc(out **url.URL) func(string) error {
	return func(arg string) error {
		if u, err := url.Parse(arg); err != nil {
			return err
		} else {
			*out = u
			return nil
		}
	}
}

func main() {
	var flags struct {
		submission *url.URL
		monitoring *url.URL
		db         string
		listen     []string
	}
	flag.StringVar(&flags.db, "db", "", "`PATH` to database file")
	flag.Func("submission", "Submission prefix `URL`", parseURLFunc(&flags.submission))
	flag.Func("monitoring", "Monitoring prefix `URL`", parseURLFunc(&flags.monitoring))
	flag.Func("listen", "`SOCKET` to listen on, in go-listener syntax (repeatable)", func(arg string) error {
		flags.listen = append(flags.listen, arg)
		return nil
	})
	flag.Parse()

	if flags.db == "" {
		log.Fatal("-db flag required")
	}
	if flags.submission == nil {
		log.Fatal("-submission flag required")
	}
	if flags.monitoring == nil {
		log.Fatal("-monitoring flag required")
	}
	if len(flags.listen) == 0 {
		log.Fatal("At least one -listen flag required")
	}

	log.SetPrefix(flags.monitoring.String() + " ")

	server, err := proxy.NewServer(flags.db, flags.submission, flags.monitoring)
	if err != nil {
		log.Fatal(err)
	}

	httpServer := http.Server{
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  30 * time.Second,
		Handler:      server,
	}

	listeners, err := listener.OpenAll(flags.listen)
	if err != nil {
		log.Fatal(err)
	}
	defer listener.CloseAll(listeners)

	for _, l := range listeners {
		go func(l net.Listener) {
			log.Fatal(httpServer.Serve(l))
		}(l)
	}

	log.Fatal(server.Run())
}
