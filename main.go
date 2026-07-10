package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync/atomic"
)

type apiConfig struct {
	fileServerHits atomic.Int32
}
type errorResponse struct {
	Error string `json:"error"`
}

func (cfg *apiConfig) middlewareMetricsInc(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg.fileServerHits.Add(1)
		next.ServeHTTP(w, r)
	})
}

func (cfg *apiConfig) handlerMetrics(w http.ResponseWriter, r *http.Request) {
	hits := cfg.fileServerHits.Load()
	htmlTemplate := `<html>
  						<body>
    						<h1>Welcome, Chirpy Admin</h1>
    						<p>Chirpy has been visited %d times!</p>
  						</body>
					</html>`
	htmlString := fmt.Sprintf(htmlTemplate, hits)
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(htmlString))
}

func (cfg *apiConfig) handlerReset(w http.ResponseWriter, r *http.Request) {
	cfg.fileServerHits.Store(0)
	w.WriteHeader(http.StatusOK)
}

func HandlerReadiness(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func respondWithError(w http.ResponseWriter, msg string, statusCode int) {
	errorBody := errorResponse{}
	errorBody.Error = msg
	respondWithJSON(w, statusCode, errorBody)
}

func respondWithJSON(w http.ResponseWriter, statusCode int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	dat, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Error marshalling JSON: %s", err)
		w.WriteHeader(500)
		return
	}
	w.WriteHeader(statusCode)
	w.Write(dat)
}

func HandlerValidateChirp(w http.ResponseWriter, r *http.Request) {
	type validateChirpRequest struct {
		Body string `json:"body"`
	}
	type validateChirpResponse struct {
		Valid bool `json:"valid"`
	}
	reqBody := validateChirpRequest{}
	respBody := validateChirpResponse{}
	err := json.NewDecoder(r.Body).Decode(&reqBody)
	if err != nil {
		log.Printf("Error decoding parameters: %s", err)
		respondWithError(w, "Something went wrong", http.StatusInternalServerError)
		return
	}
	if len(reqBody.Body) > 140 {
		respondWithError(w, "Chirp is too long", http.StatusBadRequest)
		return
	}
	respBody.Valid = true
	respondWithJSON(w, http.StatusOK, respBody)
}

func main() {
	mux := http.NewServeMux()
	cfg := &apiConfig{}

	fileServer := http.FileServer(http.Dir("."))

	mux.Handle("/app/", cfg.middlewareMetricsInc(http.StripPrefix("/app/", fileServer)))
	mux.HandleFunc("GET /api/healthz", HandlerReadiness)
	mux.HandleFunc("GET /admin/metrics", cfg.handlerMetrics)
	mux.HandleFunc("POST /admin/reset", cfg.handlerReset)
	mux.HandleFunc("POST /api/validate_chirp", HandlerValidateChirp)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/app/", http.StatusSeeOther)
	})

	server := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}

	server.ListenAndServe()
}
