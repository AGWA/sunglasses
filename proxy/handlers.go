package proxy

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"golang.org/x/mod/sumdb/tlog"
	"net/http"
	"net/url"
	"strconv"
)

func (srv *Server) getSTH(w http.ResponseWriter, req *http.Request) {
	sth := srv.sth.Load()
	if sth == nil {
		http.Error(w, "not yet synchronized with upstream log", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(sth)
}

func (srv *Server) getSTHConsistency(w http.ResponseWriter, req *http.Request) {
	query, err := url.ParseQuery(req.URL.RawQuery)
	if err != nil {
		http.Error(w, "Invalid query string: "+err.Error(), http.StatusBadRequest)
		return
	}
	first, err := strconv.ParseUint(query.Get("first"), 10, 64)
	if err != nil {
		http.Error(w, "Invalid first parameter: "+err.Error(), http.StatusBadRequest)
		return
	}
	second, err := strconv.ParseUint(query.Get("second"), 10, 64)
	if err != nil {
		http.Error(w, "Invalid second parameter: "+err.Error(), http.StatusBadRequest)
		return
	}
	if second <= first {
		http.Error(w, "second is not after first", http.StatusBadRequest)
		return
	}
	sth := srv.sth.Load()
	if sth == nil {
		http.Error(w, "not yet synchronized with upstream log", http.StatusServiceUnavailable)
		return
	}
	if first >= sth.TreeSize {
		http.Error(w, fmt.Sprintf("first is beyond the current tree size (%d)", sth.TreeSize), http.StatusBadRequest)
		return
	}
	if second >= sth.TreeSize {
		http.Error(w, fmt.Sprintf("second is beyond the current tree size (%d)", sth.TreeSize), http.StatusBadRequest)
		return
	}

	proof, err := tlog.ProveTree(int64(second), int64(first), srv.hashReader(req.Context(), sth))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(struct {
		Consistency []tlog.Hash `json:"consistency"`
	}{
		Consistency: proof,
	})
}

func (srv *Server) getProofByHash(w http.ResponseWriter, req *http.Request) {
	query, err := url.ParseQuery(req.URL.RawQuery)
	if err != nil {
		http.Error(w, "Invalid query string: "+err.Error(), http.StatusBadRequest)
		return
	}
	hash, err := base64.StdEncoding.DecodeString(query.Get("hash"))
	if err != nil {
		http.Error(w, "Invalid hash parameter: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(hash) != merkleHashLen {
		http.Error(w, fmt.Sprintf("Invalid hash parameter: wrong length (should be %d bytes long, not %d)", merkleHashLen, len(hash)), http.StatusBadRequest)
		return
	}
	treeSize, err := strconv.ParseUint(query.Get("tree_size"), 10, 64)
	if err != nil {
		http.Error(w, "Invalid tree_size parameter: "+err.Error(), http.StatusBadRequest)
		return
	}
	leafIndex, hashFound, err := srv.loadUint64(leafBucket, hash[:])
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !hashFound {
		http.Error(w, "hash not found", http.StatusBadRequest)
		return
	}
	if leafIndex >= treeSize {
		http.Error(w, "hash is not within tree_size", http.StatusBadRequest)
		return
	}
	sth := srv.sth.Load()
	if sth == nil {
		http.Error(w, "not yet synchronized with upstream log", http.StatusServiceUnavailable)
		return
	}
	if treeSize > sth.TreeSize {
		http.Error(w, fmt.Sprintf("tree_size is beyond the current tree size (%d)", sth.TreeSize), http.StatusBadRequest)
		return
	}
	proof, err := tlog.ProveRecord(int64(treeSize), int64(leafIndex), srv.hashReader(req.Context(), sth))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(struct {
		LeafIndex uint64      `json:"leaf_index"`
		AuditPath []tlog.Hash `json:"audit_path"`
	}{
		LeafIndex: leafIndex,
		AuditPath: proof,
	})
}

func (srv *Server) getEntries(w http.ResponseWriter, req *http.Request) {
	query, err := url.ParseQuery(req.URL.RawQuery)
	if err != nil {
		http.Error(w, "Invalid query string: "+err.Error(), http.StatusBadRequest)
		return
	}
	start, err := strconv.ParseUint(query.Get("start"), 10, 64)
	if err != nil {
		http.Error(w, "Invalid start parameter: "+err.Error(), http.StatusBadRequest)
		return
	}
	end, err := strconv.ParseUint(query.Get("end"), 10, 64)
	if err != nil {
		http.Error(w, "Invalid end parameter: "+err.Error(), http.StatusBadRequest)
		return
	}
	if end < start {
		http.Error(w, "end is before start", http.StatusBadRequest)
		return
	}
	sth := srv.sth.Load()
	if sth == nil {
		http.Error(w, "not yet synchronized with upstream log", http.StatusServiceUnavailable)
		return
	}
	if start >= sth.TreeSize {
		http.Error(w, fmt.Sprintf("start is beyond the current tree size (%d)", sth.TreeSize), http.StatusBadRequest)
		return
	}
	if end >= sth.TreeSize {
		http.Error(w, fmt.Sprintf("end is beyond the current tree size (%d)", sth.TreeSize), http.StatusBadRequest)
		return
	}

	entries, err := srv.downloadEntries(req.Context(), sth, start, end+1)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(struct {
		Entries []getEntriesItem `json:"entries"`
	}{
		Entries: entries,
	})
}

func (srv *Server) getEntryAndProof(w http.ResponseWriter, req *http.Request) {
	query, err := url.ParseQuery(req.URL.RawQuery)
	if err != nil {
		http.Error(w, "Invalid query string: "+err.Error(), http.StatusBadRequest)
		return
	}
	leafIndex, err := strconv.ParseUint(query.Get("leaf_index"), 10, 64)
	if err != nil {
		http.Error(w, "Invalid leaf_index parameter: "+err.Error(), http.StatusBadRequest)
		return
	}
	treeSize, err := strconv.ParseUint(query.Get("tree_size"), 10, 64)
	if err != nil {
		http.Error(w, "Invalid tree_size parameter: "+err.Error(), http.StatusBadRequest)
		return
	}
	if leafIndex >= treeSize {
		http.Error(w, "leaf_index is not within tree_size", http.StatusBadRequest)
		return
	}
	sth := srv.sth.Load()
	if sth == nil {
		http.Error(w, "not yet synchronized with upstream log", http.StatusServiceUnavailable)
		return
	}
	if treeSize > sth.TreeSize {
		http.Error(w, fmt.Sprintf("tree_size is beyond the current tree size (%d)", sth.TreeSize), http.StatusBadRequest)
		return
	}

	entries, err := srv.downloadEntries(req.Context(), sth, leafIndex, leafIndex+1)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	proof, err := tlog.ProveRecord(int64(treeSize), int64(leafIndex), srv.hashReader(req.Context(), sth))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(struct {
		LeafInput []byte      `json:"leaf_input"`
		ExtraData []byte      `json:"extra_data"`
		AuditPath []tlog.Hash `json:"audit_path"`
	}{
		LeafInput: entries[0].LeafInput,
		ExtraData: entries[0].ExtraData,
		AuditPath: proof,
	})
}
