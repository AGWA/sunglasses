package proxy

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"github.com/boltdb/bolt"
	"golang.org/x/mod/sumdb/tlog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"time"
)

const tileHeight = 8
const entriesPerTile = 1 << tileHeight
const merkleHashLen = 32

var (
	stateBucket  = []byte("state")
	leafBucket   = []byte("leaf")
	issuerBucket = []byte("issuer")
)

var (
	sthKey      = []byte("sth")
	positionKey = []byte("position")
)

type Server struct {
	db               *bolt.DB
	monitoringPrefix *url.URL
	mux              *http.ServeMux
	sth              atomic.Pointer[signedTreeHead]
}

type Config struct {
	DBPath           string
	SubmissionPrefix *url.URL
	MonitoringPrefix *url.URL
	UnsafeNoFsync    bool
}

func NewServer(config *Config) (*Server, error) {
	db, err := bolt.Open(config.DBPath, 0666, &bolt.Options{Timeout: 30 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("error opening database: %w", err)
	}
	defer func() {
		if db != nil {
			db.Close()
		}
	}()
	db.NoSync = config.UnsafeNoFsync
	var sthBytes []byte
	if err := db.Update(func(tx *bolt.Tx) error {
		state, _ := tx.CreateBucketIfNotExists(stateBucket)
		sthBytes = state.Get(sthKey)
		tx.CreateBucketIfNotExists(issuerBucket)
		tx.CreateBucketIfNotExists(leafBucket)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("error preparing database: %w", err)
	}
	server := &Server{
		monitoringPrefix: config.MonitoringPrefix,
		mux:              http.NewServeMux(),
	}
	if sthBytes != nil {
		sth := new(signedTreeHead)
		if err := json.Unmarshal(sthBytes, sth); err != nil {
			return nil, fmt.Errorf("STH stored in database is corrupted: %w", err)
		}
		server.sth.Store(sth)
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

	server.db = db
	db = nil // prevent defer from closing db

	return server, nil
}

func (srv *Server) store(bucket []byte, key []byte, value []byte) (err error) {
	err = srv.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucket).Put(key, value)
	})
	return
}

func (srv *Server) load(bucket []byte, key []byte) (value []byte, err error) {
	err = srv.db.View(func(tx *bolt.Tx) error {
		value = tx.Bucket(bucket).Get(key)
		return nil
	})
	return
}

func (srv *Server) loadUint64(bucket []byte, key []byte) (uint64, bool, error) {
	valueBytes, err := srv.load(bucket, key)
	if err != nil {
		return 0, false, err
	}
	if valueBytes == nil {
		return 0, false, nil
	}
	if len(valueBytes) != 8 {
		return 0, false, fmt.Errorf("value has wrong length for uint64")
	}
	return binary.BigEndian.Uint64(valueBytes), true, nil
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
