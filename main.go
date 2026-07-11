package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/lain-the-coder/chirpy/internal/database"
	_ "github.com/lib/pq"
)

type apiConfig struct {
	fileServerHits atomic.Int32
	db             *database.Queries
	platform       string
}

// declaring error response struct globally for free use
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
	if cfg.platform != "dev" {
		respondWithError(w, "Forbidden", http.StatusForbidden)
		return
	}
	err := cfg.db.ResetUser(r.Context())
	if err != nil {
		log.Printf("Error deleting all user records: %s", err)
		// delegating error structuring to helper function
		respondWithError(w, "Something went wrong", http.StatusInternalServerError)
		return
	}
	cfg.fileServerHits.Store(0)
	w.WriteHeader(http.StatusOK)
}

func HandlerReadiness(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

// generic helper function for error construction
func respondWithError(w http.ResponseWriter, msg string, statusCode int) {
	errorBody := errorResponse{}
	errorBody.Error = msg
	// delegating json construction to helper function
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

func cleanString(sentence string, replacements []string) string {
	words := strings.Split(sentence, " ")
	for i := range words {
		for _, replacement := range replacements {
			if strings.ToLower(words[i]) == replacement {
				words[i] = "****"
				break // once a match for that word is found then break this loop and check next work immediately; performance save
			}
		}
	}
	newSentence := strings.Join(words, " ")
	return newSentence
}

func HandlerValidateChirp(w http.ResponseWriter, r *http.Request) {
	type validateChirpRequest struct {
		Body string `json:"body"`
	}
	type validateChirpResponse struct {
		CleanedBody string `json:"cleaned_body"`
	}
	reqBody := validateChirpRequest{}
	respBody := validateChirpResponse{}
	err := json.NewDecoder(r.Body).Decode(&reqBody)
	if err != nil {
		log.Printf("Error decoding parameters: %s", err)
		// delegating error structuring to helper function
		respondWithError(w, "Something went wrong", http.StatusInternalServerError)
		return
	}
	// character length validation
	if len(reqBody.Body) > 140 {
		respondWithError(w, "Chirp is too long", http.StatusBadRequest)
		return
	}
	// profanity validation
	replacements := []string{"kerfuffle", "sharbert", "fornax"}
	respBody.CleanedBody = cleanString(reqBody.Body, replacements)
	//delegating json construction to helper function
	respondWithJSON(w, http.StatusOK, respBody)
}

func (cfg *apiConfig) HandlerCreateUser(w http.ResponseWriter, r *http.Request) {
	type createUserRequest struct {
		Email string `json:"email"`
	}
	type createUserResponse struct {
		ID        uuid.UUID `json:"id"`
		CreatedAt time.Time `json:"created_at"`
		UpdatedAt time.Time `json:"updated_at"`
		Email     string    `json:"email"`
	}
	reqBody := createUserRequest{}
	err := json.NewDecoder(r.Body).Decode(&reqBody)
	if err != nil {
		log.Printf("Error decoding parameters: %s", err)
		// delegating error structuring to helper function
		respondWithError(w, "Something went wrong", http.StatusInternalServerError)
		return
	}
	reqBody.Email = strings.TrimSpace(reqBody.Email)
	if reqBody.Email == "" {
		log.Printf("Email is blank")
		respondWithError(w, "Email cannot be blank", http.StatusBadRequest)
		return
	}
	user, err := cfg.db.CreateUser(r.Context(), reqBody.Email)
	if err != nil {
		log.Printf("Error inserting record into database: %s", err)
		// delegating error structuring to helper function
		respondWithError(w, "Something went wrong", http.StatusInternalServerError)
		return
	}
	resBody := createUserResponse{
		ID:        user.ID,
		CreatedAt: user.CreatedAt,
		UpdatedAt: user.UpdatedAt,
		Email:     user.Email,
	}
	respondWithJSON(w, http.StatusCreated, resBody)
}

func main() {
	mux := http.NewServeMux()

	err := godotenv.Load()
	if err != nil {
		log.Fatal("Error loading .env file")
	}
	dbURL := os.Getenv("DB_URL")
	platform := os.Getenv("PLATFORM")
	rawDB, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Fatalf("error opening database: %v", err)
	}
	db := database.New(rawDB)

	cfg := &apiConfig{
		db:       db,
		platform: platform,
	}

	fileServer := http.FileServer(http.Dir("."))

	mux.Handle("/app/", cfg.middlewareMetricsInc(http.StripPrefix("/app/", fileServer)))
	mux.HandleFunc("GET /api/healthz", HandlerReadiness)
	mux.HandleFunc("GET /admin/metrics", cfg.handlerMetrics)
	mux.HandleFunc("POST /admin/reset", cfg.handlerReset)
	mux.HandleFunc("POST /api/validate_chirp", HandlerValidateChirp)
	mux.HandleFunc("POST /api/users", cfg.HandlerCreateUser)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/app/", http.StatusSeeOther)
	})

	server := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}

	server.ListenAndServe()
}
