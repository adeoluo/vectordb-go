// Package server exposes a db.DB over an HTTP JSON API.
package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/adeoluo/vectordb-go/internal/db"
)

// MaxBodyBytes bounds request bodies.
const MaxBodyBytes = 64 << 20

// Server routes HTTP requests to a DB.
type Server struct {
	db      *db.DB
	logger  *slog.Logger
	metrics *metrics
	mux     *http.ServeMux
}

// New builds the HTTP handler tree for d.
func New(d *db.DB, logger *slog.Logger) *Server {
	s := &Server{db: d, logger: logger, metrics: newMetrics(), mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /health", s.health)
	s.mux.HandleFunc("GET /metrics", s.metricsHandler)
	s.mux.HandleFunc("GET /v1/collections", s.listCollections)
	s.mux.HandleFunc("POST /v1/collections", s.createCollection)
	s.mux.HandleFunc("GET /v1/collections/{name}", s.getCollection)
	s.mux.HandleFunc("DELETE /v1/collections/{name}", s.dropCollection)
	s.mux.HandleFunc("POST /v1/collections/{name}/upsert", s.upsert)
	s.mux.HandleFunc("POST /v1/collections/{name}/delete", s.deleteVectors)
	s.mux.HandleFunc("POST /v1/collections/{name}/search", s.search)
	s.mux.HandleFunc("GET /v1/collections/{name}/vectors/{id}", s.getVector)
	s.mux.HandleFunc("POST /v1/collections/{name}/snapshot", s.snapshot)
	return s
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

// ServeHTTP records per-route metrics around the mux.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
	r.Body = http.MaxBytesReader(sw, r.Body, MaxBodyBytes)
	s.mux.ServeHTTP(sw, r)
	route := r.Pattern
	if route == "" {
		route = "unmatched"
	}
	s.metrics.observe(route, sw.code, time.Since(start))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (s *Server) writeError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	var maxErr *http.MaxBytesError
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	switch {
	case errors.Is(err, db.ErrInvalid), errors.As(err, &syntaxErr), errors.As(err, &typeErr), errors.Is(err, errBadJSON):
		code = http.StatusBadRequest
	case errors.As(err, &maxErr):
		code = http.StatusRequestEntityTooLarge
	case errors.Is(err, db.ErrNotFound), errors.Is(err, errVectorNotFound):
		code = http.StatusNotFound
	case errors.Is(err, db.ErrExists):
		code = http.StatusConflict
	case errors.Is(err, db.ErrClosed):
		code = http.StatusServiceUnavailable
	}
	if code == http.StatusInternalServerError {
		s.logger.Error("request failed", "err", err)
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

var (
	errBadJSON        = errors.New("invalid JSON body")
	errVectorNotFound = errors.New("vector not found")
)

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return err
		}
		return errors.Join(errBadJSON, err)
	}
	return nil
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) metricsHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s.metrics.write(w, s.db)
}

func (s *Server) listCollections(w http.ResponseWriter, _ *http.Request) {
	out := []db.Stats{}
	for _, c := range s.db.List() {
		out = append(out, c.Stats())
	}
	writeJSON(w, http.StatusOK, map[string]any{"collections": out})
}

type createRequest struct {
	Name           string `json:"name"`
	Dim            int    `json:"dim"`
	Metric         string `json:"metric"`
	M              int    `json:"m"`
	EfConstruction int    `json:"ef_construction"`
}

func (s *Server) createCollection(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := decode(r, &req); err != nil {
		s.writeError(w, err)
		return
	}
	c, err := s.db.CreateCollection(req.Name, db.CollectionConfig{
		Dim: req.Dim, Metric: req.Metric, M: req.M, EfConstruction: req.EfConstruction,
	})
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, c.Stats())
}

func (s *Server) collection(w http.ResponseWriter, r *http.Request) *db.Collection {
	c, err := s.db.Collection(r.PathValue("name"))
	if err != nil {
		s.writeError(w, err)
		return nil
	}
	return c
}

func (s *Server) getCollection(w http.ResponseWriter, r *http.Request) {
	if c := s.collection(w, r); c != nil {
		writeJSON(w, http.StatusOK, c.Stats())
	}
}

func (s *Server) dropCollection(w http.ResponseWriter, r *http.Request) {
	if err := s.db.DropCollection(r.PathValue("name")); err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"dropped": true})
}

type vectorJSON struct {
	ID     string    `json:"id"`
	Values []float32 `json:"values"`
}

type upsertRequest struct {
	Vectors []vectorJSON `json:"vectors"`
}

func (s *Server) upsert(w http.ResponseWriter, r *http.Request) {
	c := s.collection(w, r)
	if c == nil {
		return
	}
	var req upsertRequest
	if err := decode(r, &req); err != nil {
		s.writeError(w, err)
		return
	}
	items := make([]db.Item, len(req.Vectors))
	for i, v := range req.Vectors {
		items[i] = db.Item{ID: v.ID, Vector: v.Values}
	}
	lsn, err := c.Upsert(items)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"upserted": len(items), "lsn": lsn})
}

type deleteRequest struct {
	IDs []string `json:"ids"`
}

func (s *Server) deleteVectors(w http.ResponseWriter, r *http.Request) {
	c := s.collection(w, r)
	if c == nil {
		return
	}
	var req deleteRequest
	if err := decode(r, &req); err != nil {
		s.writeError(w, err)
		return
	}
	lsn, n, err := c.Delete(req.IDs)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": n, "lsn": lsn})
}

type searchRequest struct {
	Vector []float32 `json:"vector"`
	K      int       `json:"k"`
	Ef     int       `json:"ef"`
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	c := s.collection(w, r)
	if c == nil {
		return
	}
	req := searchRequest{K: 10}
	if err := decode(r, &req); err != nil {
		s.writeError(w, err)
		return
	}
	start := time.Now()
	hits, err := c.Search(req.Vector, req.K, req.Ef)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"results": hits,
		"took_ms": float64(time.Since(start).Microseconds()) / 1000,
	})
}

func (s *Server) getVector(w http.ResponseWriter, r *http.Request) {
	c := s.collection(w, r)
	if c == nil {
		return
	}
	id := r.PathValue("id")
	v, ok := c.Get(id)
	if !ok {
		s.writeError(w, errVectorNotFound)
		return
	}
	writeJSON(w, http.StatusOK, vectorJSON{ID: id, Values: v})
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	c := s.collection(w, r)
	if c == nil {
		return
	}
	lsn, err := c.Snapshot()
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshot_lsn": lsn})
}
