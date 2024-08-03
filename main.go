package main

import (
	"encoding/base64"
	"flag"
	"fmt"
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

func parseLogIDFunc(out *proxy.LogID) func(string) error {
	return func(arg string) error {
		if b, err := base64.StdEncoding.DecodeString(arg); err != nil {
			return err
		} else if len(b) != 32 {
			return fmt.Errorf("wrong length for Log ID")
		} else {
			*out = (proxy.LogID)(b)
			return nil
		}
	}
}

func main() {
	var flags struct {
		submission    *url.URL
		monitoring    *url.URL
		id            proxy.LogID
		db            string
		listen        []string
		unsafeNoFsync bool
		noLeafIndex   bool
	}
	flag.StringVar(&flags.db, "db", "", "`PATH` to database file (will be created if necessary)")
	flag.Func("id", "Log ID `BASE64`", parseLogIDFunc(&flags.id))
	flag.Func("submission", "Submission prefix `URL`", parseURLFunc(&flags.submission))
	flag.Func("monitoring", "Monitoring prefix `URL`", parseURLFunc(&flags.monitoring))
	flag.Func("listen", "`SOCKET` to listen on, in go-listener syntax (repeatable)", func(arg string) error {
		flags.listen = append(flags.listen, arg)
		return nil
	})
	flag.BoolVar(&flags.unsafeNoFsync, "unsafe-nofsync", false, "disable database fsync (unsafe; only appropriate during initial indexing)")
	flag.BoolVar(&flags.noLeafIndex, "no-leaf-index", false, "disable leaf indexing (get-proof-by-hash endpoint won't work)")
	flag.Parse()

	if flags.db == "" {
		log.Fatal("-db flag required")
	}
	if flags.id == (proxy.LogID{}) {
		log.Fatal("-id flag required")
	}
	if flags.submission == nil {
		log.Fatal("-submission flag required")
	}
	if flags.monitoring == nil {
		log.Fatal("-monitoring flag required")
	}

	log.SetPrefix(flags.monitoring.String() + " ")

	server, err := proxy.NewServer(&proxy.Config{
		LogID:            flags.id,
		DBPath:           flags.db,
		SubmissionPrefix: flags.submission,
		MonitoringPrefix: flags.monitoring,
		UnsafeNoFsync:    flags.unsafeNoFsync,
		DisableLeafIndex: flags.noLeafIndex,
	})
	if err != nil {
		log.Fatal(err)
	}

	httpServer := http.Server{
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  30 * time.Second,
		Handler:      http.MaxBytesHandler(server, 128*1024),
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
