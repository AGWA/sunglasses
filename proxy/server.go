package proxy

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/mod/sumdb/tlog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"src.agwa.name/go-dbutil/dbschema"
	"src.agwa.name/sunglasses/proxy/schema"
	"sync/atomic"
)

const tileHeight = 8
const entriesPerTile = 1 << tileHeight
const merkleHashLen = 32

type LogID [32]byte

type Server struct {
	logID            LogID
	db               *sql.DB
	monitoringPrefix *url.URL
	mux              *http.ServeMux
	sth              atomic.Pointer[signedTreeHead]
	disableLeafIndex bool
}

type Config struct {
	LogID            LogID
	DBPath           string
	SubmissionPrefix *url.URL
	MonitoringPrefix *url.URL
	UnsafeNoFsync    bool
	DisableLeafIndex bool
}

func NewServer(config *Config) (*Server, error) {
	// Use NORMAL instead of FULL for better write performance (30%
	// improvement in throughput per one source). We may lose recent commits if
	// there's a power loss, but we can easily recreate the data the next time
	// Sunglasses runs. The database will not be corrupted.
	synchronous := "NORMAL"
	if config.UnsafeNoFsync {
		// Database can be corrupted if there's a power failure, but commits
		// can be "orders of magnitude" faster.
		synchronous = "OFF"
	}
	server := &Server{
		logID:            config.LogID,
		monitoringPrefix: config.MonitoringPrefix,
		mux:              http.NewServeMux(),
		disableLeafIndex: config.DisableLeafIndex,
	}
	submissionProxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(config.SubmissionPrefix)
		},
	}
	server.mux.Handle("POST /ct/v1/add-chain", submissionProxy)
	server.mux.Handle("POST /ct/v1/add-pre-chain", submissionProxy)
	server.mux.HandleFunc("GET /ct/v1/get-sth", server.getSTH)
	server.mux.HandleFunc("GET /ct/v1/get-sth-consistency", server.getSTHConsistency)
	server.mux.HandleFunc("GET /ct/v1/get-proof-by-hash", server.getProofByHash)
	server.mux.HandleFunc("GET /ct/v1/get-entries", server.getEntries)
	server.mux.Handle("GET /ct/v1/get-roots", submissionProxy)
	server.mux.HandleFunc("GET /ct/v1/get-entry-and-proof", server.getEntryAndProof)

	if config.DBPath != "" {
		db, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?_busy_timeout=5000&_foreign_keys=ON&_txlock=immediate&_journal_mode=WAL&_synchronous=%s", url.PathEscape(config.DBPath), url.PathEscape(synchronous)))
		if err != nil {
			return nil, fmt.Errorf("error opening database: %w", err)
		}
		defer func() {
			if db != nil {
				db.Close()
			}
		}()
		if err := dbschema.Build(context.Background(), db, schema.Files); err != nil {
			return nil, fmt.Errorf("error building database schema: %w", err)
		}

		var sthBytes []byte
		if err := db.QueryRow(`SELECT sth FROM state`).Scan(&sthBytes); err != nil {
			return nil, fmt.Errorf("error loading STH from database: %w", err)
		}
		if sthBytes != nil {
			sth := new(signedTreeHead)
			if err := json.Unmarshal(sthBytes, sth); err != nil {
				return nil, fmt.Errorf("STH stored in database is corrupted: %w", err)
			}
			server.sth.Store(sth)
		}
		server.db = db
		db = nil // prevent defer from closing db
	}

	return server, nil
}

func (srv *Server) tileReader(ctx context.Context) tlog.TileReader {
	return &tileReader{ctx: ctx, prefix: srv.monitoringPrefix}
}

func (srv *Server) hashReader(ctx context.Context, sth *signedTreeHead) tlog.HashReader {
	return tlog.TileHashReader(sth.tlogTree(), srv.tileReader(ctx))
}

func (srv *Server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	srv.mux.ServeHTTP(w, req)
}
