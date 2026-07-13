package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/lain-the-coder/chirpy/internal/auth"
	"github.com/lain-the-coder/chirpy/internal/database"
	_ "github.com/lib/pq"
)

type apiConfig struct {
	fileServerHits atomic.Int32
	db             *database.Queries
	platform       string
	secret         string
	apiKey         string
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

func (cfg *apiConfig) HandlerCreateUser(w http.ResponseWriter, r *http.Request) {
	type createUserRequest struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	type createUserResponse struct {
		ID          uuid.UUID `json:"id"`
		CreatedAt   time.Time `json:"created_at"`
		UpdatedAt   time.Time `json:"updated_at"`
		Email       string    `json:"email"`
		IsChirpyRed bool      `json:"is_chirpy_red"`
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
	if reqBody.Password == "" {
		log.Printf("Password is blank")
		respondWithError(w, "Password cannot be blank", http.StatusBadRequest)
		return
	}
	hashedPassword, err := auth.HashPassword(reqBody.Password)
	if err != nil {
		log.Printf("Error hashing password: %s", err)
		// delegating error structuring to helper function
		respondWithError(w, "Something went wrong", http.StatusInternalServerError)
		return
	}
	user, err := cfg.db.CreateUser(r.Context(), database.CreateUserParams{
		Email:          reqBody.Email,
		HashedPassword: hashedPassword,
	})
	if err != nil {
		log.Printf("Error inserting record into database: %s", err)
		// delegating error structuring to helper function
		respondWithError(w, "Something went wrong", http.StatusInternalServerError)
		return
	}
	resBody := createUserResponse{
		ID:          user.ID,
		CreatedAt:   user.CreatedAt,
		UpdatedAt:   user.UpdatedAt,
		Email:       user.Email,
		IsChirpyRed: false,
	}
	respondWithJSON(w, http.StatusCreated, resBody)
}

func (cfg *apiConfig) HandleCreateChirp(w http.ResponseWriter, r *http.Request) {
	type CreateChirpRequest struct {
		Body string `json:"body"`
	}
	type CreateChirpResponse struct {
		ID        uuid.UUID `json:"id"`
		CreatedAt time.Time `json:"created_at"`
		UpdatedAt time.Time `json:"updated_at"`
		Body      string    `json:"body"`
		UserID    uuid.UUID `json:"user_id"`
	}
	// prior to decoding request body check for valid auth header otherwise short circuit
	authToken, err := auth.GetBearerToken(r.Header)
	if err != nil {
		log.Printf("Error getting bearer token: %s", err)
		respondWithError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	IDPostValidation, err := auth.ValidateJWT(authToken, cfg.secret)
	if err != nil {
		log.Printf("Error validating token: %s", err)
		respondWithError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	reqBody := CreateChirpRequest{}
	err = json.NewDecoder(r.Body).Decode(&reqBody)
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
	reqBody.Body = cleanString(reqBody.Body, replacements)
	reqBody.Body = strings.TrimSpace(reqBody.Body)
	if reqBody.Body == "" {
		log.Printf("Body is blank")
		respondWithError(w, "Body cannot be blank", http.StatusBadRequest)
		return
	}
	chirp, err := cfg.db.CreateChirp(r.Context(), database.CreateChirpParams{
		Body:   reqBody.Body,
		UserID: IDPostValidation,
	})
	if err != nil {
		log.Printf("Error inserting record into database: %s", err)
		// delegating error structuring to helper function
		respondWithError(w, "Something went wrong", http.StatusInternalServerError)
		return
	}
	resBody := CreateChirpResponse{
		ID:        chirp.ID,
		CreatedAt: chirp.CreatedAt,
		UpdatedAt: chirp.UpdatedAt,
		Body:      chirp.Body,
		UserID:    chirp.UserID,
	}
	respondWithJSON(w, http.StatusCreated, resBody)
}

func (cfg *apiConfig) HandleGetChirps(w http.ResponseWriter, r *http.Request) {
	authorIDStr := r.URL.Query().Get("author_id")

	var data []database.Chirp
	var err error
	if authorIDStr == "" {
		data, err = cfg.db.GetChirps(r.Context())
	} else {
		authorID, parseErr := uuid.Parse(authorIDStr)
		if parseErr != nil {
			log.Printf("Error parsing author_id: %s", parseErr)
			respondWithError(w, "Invalid author_id", http.StatusBadRequest)
			return
		}
		data, err = cfg.db.GetChirpsByAuthor(r.Context(), authorID)
	}
	if err != nil {
		log.Printf("Error retrieving records from database: %s", err)
		// delegating error structuring to helper function
		respondWithError(w, "Something went wrong", http.StatusInternalServerError)
		return
	}
	type GetChirpResponse struct {
		ID        uuid.UUID `json:"id"`
		CreatedAt time.Time `json:"created_at"`
		UpdatedAt time.Time `json:"updated_at"`
		Body      string    `json:"body"`
		UserID    uuid.UUID `json:"user_id"`
	}
	chirps := make([]GetChirpResponse, len(data))
	for i := range data {
		chirps[i].ID = data[i].ID
		chirps[i].CreatedAt = data[i].CreatedAt
		chirps[i].UpdatedAt = data[i].UpdatedAt
		chirps[i].Body = data[i].Body
		chirps[i].UserID = data[i].UserID
	}
	respondWithJSON(w, http.StatusOK, chirps)
}

func (cfg *apiConfig) HandleGetChirp(w http.ResponseWriter, r *http.Request) {
	chirpIDStr := r.PathValue("chirpID")
	chirpID, err := uuid.Parse(chirpIDStr)
	if err != nil {
		log.Printf("Error parsing chirp ID: %s", err)
		respondWithError(w, "Invalid chirp ID", http.StatusBadRequest)
		return
	}

	chirp, err := cfg.db.GetChirp(r.Context(), chirpID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			respondWithError(w, "Chirp not found", http.StatusNotFound)
			return
		}
		log.Printf("Error retrieving chirp from database: %s", err)
		respondWithError(w, "Something went wrong", http.StatusInternalServerError)
		return
	}

	type GetChirpResponse struct {
		ID        uuid.UUID `json:"id"`
		CreatedAt time.Time `json:"created_at"`
		UpdatedAt time.Time `json:"updated_at"`
		Body      string    `json:"body"`
		UserID    uuid.UUID `json:"user_id"`
	}
	resBody := GetChirpResponse{
		ID:        chirp.ID,
		CreatedAt: chirp.CreatedAt,
		UpdatedAt: chirp.UpdatedAt,
		Body:      chirp.Body,
		UserID:    chirp.UserID,
	}
	respondWithJSON(w, http.StatusOK, resBody)
}

func (cfg *apiConfig) HandlerLoginUser(w http.ResponseWriter, r *http.Request) {
	type LoginUserRequest struct {
		Password string `json:"password"`
		Email    string `json:"email"`
	}
	type createUserResponse struct {
		ID           uuid.UUID `json:"id"`
		CreatedAt    time.Time `json:"created_at"`
		UpdatedAt    time.Time `json:"updated_at"`
		Email        string    `json:"email"`
		Token        string    `json:"token"`
		RefreshToken string    `json:"refresh_token"`
		IsChirpyRed  bool      `json:"is_chirpy_red"`
	}
	reqBody := LoginUserRequest{}
	err := json.NewDecoder(r.Body).Decode(&reqBody)
	if err != nil {
		log.Printf("Error decoding parameters: %s", err)
		// delegating error structuring to helper function
		respondWithError(w, "Something went wrong", http.StatusInternalServerError)
		return
	}
	// basic validation of email/password to save a db trip
	reqBody.Email = strings.TrimSpace(reqBody.Email)
	if reqBody.Email == "" {
		log.Printf("Email is blank")
		respondWithError(w, "Email cannot be blank", http.StatusBadRequest)
		return
	}
	if reqBody.Password == "" {
		log.Printf("Password is blank")
		respondWithError(w, "Password cannot be blank", http.StatusBadRequest)
		return
	}
	// Set a default value for the optional field
	defaultExpirationInSeconds := 3600 // 1 hour default
	defaultRefreshTokenExpiration := time.Now().UTC().Add(60 * 24 * time.Hour)
	user, err := cfg.db.GetUser(r.Context(), reqBody.Email)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// delegating error structuring to helper function
			respondWithError(w, "Incorrect email or password", http.StatusUnauthorized)
			return
		}
		log.Printf("Error retrieving user from database: %s", err)
		// delegating error structuring to helper function
		respondWithError(w, "Something went wrong", http.StatusInternalServerError)
		return
	}
	match, err := auth.CheckPasswordHash(reqBody.Password, user.HashedPassword)
	if err != nil {
		log.Printf("Error running function check for hashed password: %s", err)
		// delegating error structuring to helper function
		respondWithError(w, "Something went wrong", http.StatusInternalServerError)
		return
	}
	if !match {
		log.Printf("Incorrect password: %s", err)
		// delegating error structuring to helper function
		respondWithError(w, "Incorrect email or password", http.StatusUnauthorized)
		return
	}
	token, err := auth.MakeJWT(user.ID, cfg.secret, time.Duration(defaultExpirationInSeconds)*time.Second)
	if err != nil {
		log.Printf("Error running function for token generation: %s", err)
		// delegating error structuring to helper function
		respondWithError(w, "Something went wrong", http.StatusInternalServerError)
		return
	}
	// refresh token logic
	refreshToken := auth.MakeRefreshToken()
	_, err = cfg.db.CreateRefreshToken(r.Context(), database.CreateRefreshTokenParams{
		Token:     refreshToken,
		UserID:    user.ID,
		ExpiresAt: defaultRefreshTokenExpiration,
	})
	if err != nil {
		log.Printf("Error inserting record into database: %s", err)
		// delegating error structuring to helper function
		respondWithError(w, "Something went wrong", http.StatusInternalServerError)
		return
	}
	chirpyRed := false
	if user.IsChirpyRed.Valid {
		if user.IsChirpyRed.Bool {
			chirpyRed = true
		} else {
			chirpyRed = false
		}
	}
	resBody := createUserResponse{
		ID:           user.ID,
		CreatedAt:    user.CreatedAt,
		UpdatedAt:    user.UpdatedAt,
		Email:        user.Email,
		Token:        token,
		RefreshToken: refreshToken,
		IsChirpyRed:  chirpyRed,
	}
	respondWithJSON(w, http.StatusOK, resBody)
}

func (cfg *apiConfig) HandlerRefreshToken(w http.ResponseWriter, r *http.Request) {
	type RefreshTokenResponse struct {
		Token string `json:"token"`
	}
	refreshToken, err := auth.GetBearerToken(r.Header)
	if err != nil {
		log.Printf("Error getting bearer token: %s", err)
		respondWithError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	refreshTokenRow, err := cfg.db.GetRefreshToken(r.Context(), refreshToken)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			log.Printf("Refresh token not present in db: %s", err)
			respondWithError(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		log.Printf("Error retrieving refresh token from database: %s", err)
		respondWithError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if time.Now().UTC().After(refreshTokenRow.ExpiresAt) {
		log.Printf("Refresh token expired for user %s", refreshTokenRow.UserID)
		respondWithError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if refreshTokenRow.RevokedAt.Valid {
		log.Printf("Refresh token revoked for user %s", refreshTokenRow.UserID)
		respondWithError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	defaultExpirationInSeconds := 3600 // 1 hour default
	token, err := auth.MakeJWT(refreshTokenRow.UserID, cfg.secret, time.Duration(defaultExpirationInSeconds)*time.Second)
	if err != nil {
		log.Printf("Error running function for token generation: %s", err)
		// delegating error structuring to helper function
		respondWithError(w, "Something went wrong", http.StatusInternalServerError)
		return
	}
	resBody := RefreshTokenResponse{
		Token: token,
	}
	respondWithJSON(w, http.StatusOK, resBody)
}

func (cfg *apiConfig) HandlerRevokeRefreshToken(w http.ResponseWriter, r *http.Request) {
	refreshToken, err := auth.GetBearerToken(r.Header)
	if err != nil {
		log.Printf("Error getting bearer token: %s", err)
		respondWithError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	err = cfg.db.RevokeRefreshToken(r.Context(), refreshToken)
	if err != nil {
		log.Printf("Error updating refresh token table in db: %s", err)
		// delegating error structuring to helper function
		respondWithError(w, "Something went wrong", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (cfg *apiConfig) HandlerUpdateUser(w http.ResponseWriter, r *http.Request) {
	authToken, err := auth.GetBearerToken(r.Header)
	if err != nil {
		log.Printf("Error getting bearer token: %s", err)
		respondWithError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	IDPostValidation, err := auth.ValidateJWT(authToken, cfg.secret)
	if err != nil {
		log.Printf("Error validating token: %s", err)
		respondWithError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	type UpdateUserRequest struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	type UpdateUserResponse struct {
		Email string `json:"email"`
	}
	reqBody := UpdateUserRequest{}
	err = json.NewDecoder(r.Body).Decode(&reqBody)
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
	if reqBody.Password == "" {
		log.Printf("Password is blank")
		respondWithError(w, "Password cannot be blank", http.StatusBadRequest)
		return
	}
	hashedPassword, err := auth.HashPassword(reqBody.Password)
	if err != nil {
		log.Printf("Error hashing password: %s", err)
		// delegating error structuring to helper function
		respondWithError(w, "Something went wrong", http.StatusInternalServerError)
		return
	}
	email, err := cfg.db.UpdateUser(r.Context(), database.UpdateUserParams{
		ID:             IDPostValidation,
		Email:          reqBody.Email,
		HashedPassword: hashedPassword,
	})
	if err != nil {
		log.Printf("Error updating record into database: %s", err)
		// delegating error structuring to helper function
		respondWithError(w, "Something went wrong", http.StatusInternalServerError)
		return
	}
	resBody := UpdateUserResponse{
		Email: email,
	}
	respondWithJSON(w, http.StatusOK, resBody)
}

func (cfg *apiConfig) HandlerDeleteChirp(w http.ResponseWriter, r *http.Request) {
	authToken, err := auth.GetBearerToken(r.Header)
	if err != nil {
		log.Printf("Error getting bearer token: %s", err)
		respondWithError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	IDPostValidation, err := auth.ValidateJWT(authToken, cfg.secret)
	if err != nil {
		log.Printf("Error validating token: %s", err)
		respondWithError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	chirpIDStr := r.PathValue("chirpID")
	chirpID, err := uuid.Parse(chirpIDStr)
	if err != nil {
		log.Printf("Error parsing chirp ID: %s", err)
		respondWithError(w, "Invalid chirp ID", http.StatusBadRequest)
		return
	}
	chirp, err := cfg.db.GetChirp(r.Context(), chirpID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			respondWithError(w, "Chirp not found", http.StatusNotFound)
			return
		}
		log.Printf("Error retrieving chirp from database: %s", err)
		respondWithError(w, "Something went wrong", http.StatusInternalServerError)
		return
	}
	if chirp.UserID != IDPostValidation {
		log.Printf("Chirp's user ID and user ID from token don't match")
		respondWithError(w, "Forbidden", http.StatusForbidden)
		return
	}
	err = cfg.db.DeleteChirp(r.Context(), chirpID)
	if err != nil {
		log.Printf("Error deleting chirp from database: %s", err)
		respondWithError(w, "Something went wrong", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (cfg *apiConfig) HandlerPolkaWebhooks(w http.ResponseWriter, r *http.Request) {
	apiKey, err := auth.GetAPIKey(r.Header)
	if err != nil {
		log.Printf("Error getting api key: %s", err)
		respondWithError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if apiKey != cfg.apiKey {
		log.Printf("api key does not match")
		respondWithError(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	type DataRequest struct {
		UserID uuid.UUID `json:"user_id"`
	}
	type PolkaWebHookRequest struct {
		Event string      `json:"event"`
		Data  DataRequest `json:"data"`
	}
	reqBody := PolkaWebHookRequest{}
	err = json.NewDecoder(r.Body).Decode(&reqBody)
	if err != nil {
		log.Printf("Error decoding parameters: %s", err)
		// delegating error structuring to helper function
		respondWithError(w, "Something went wrong", http.StatusInternalServerError)
		return
	}
	// if event field is anything other than user.upgraded immediately notify Polka that webhook was received succesfully
	// no need to retry
	if reqBody.Event != "user.upgraded" {
		w.WriteHeader(http.StatusNoContent) // 204 status code
		return
	}
	_, err = cfg.db.UpgradeUser(r.Context(), reqBody.Data.UserID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			respondWithError(w, "User not found", http.StatusNotFound)
			return
		}
		log.Printf("Error updating user record in database database: %s", err)
		respondWithError(w, "Something went wrong", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func main() {
	mux := http.NewServeMux()

	err := godotenv.Load()
	if err != nil {
		log.Fatal("Error loading .env file")
	}
	dbURL := os.Getenv("DB_URL")
	platform := os.Getenv("PLATFORM")
	secret := os.Getenv("SECRET")
	apiKey := os.Getenv("POLKA_KEY")
	rawDB, err := sql.Open("postgres", dbURL)
	if err != nil {
		log.Fatalf("error opening database: %v", err)
	}
	db := database.New(rawDB)

	cfg := &apiConfig{
		db:       db,
		platform: platform,
		secret:   secret,
		apiKey:   apiKey,
	}

	fileServer := http.FileServer(http.Dir("."))

	// File serve
	mux.Handle("/app/", cfg.middlewareMetricsInc(http.StripPrefix("/app/", fileServer)))

	// Dummy endpoint
	mux.HandleFunc("GET /api/healthz", HandlerReadiness)

	// Admin endpoints
	mux.HandleFunc("GET /admin/metrics", cfg.handlerMetrics)
	mux.HandleFunc("POST /admin/reset", cfg.handlerReset)

	// Enduser endpoints
	mux.HandleFunc("POST /api/users", cfg.HandlerCreateUser)
	mux.HandleFunc("PUT /api/users", cfg.HandlerUpdateUser)
	mux.HandleFunc("POST /api/login", cfg.HandlerLoginUser)
	mux.HandleFunc("POST /api/chirps", cfg.HandleCreateChirp)
	mux.HandleFunc("GET /api/chirps", cfg.HandleGetChirps)
	mux.HandleFunc("GET /api/chirps/{chirpID}", cfg.HandleGetChirp)
	mux.HandleFunc("DELETE /api/chirps/{chirpID}", cfg.HandlerDeleteChirp)
	mux.HandleFunc("POST /api/refresh", cfg.HandlerRefreshToken)
	mux.HandleFunc("POST /api/revoke", cfg.HandlerRevokeRefreshToken)
	mux.HandleFunc("POST /api/polka/webhooks", cfg.HandlerPolkaWebhooks)

	// Homepage
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/app/", http.StatusSeeOther)
	})

	server := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}

	server.ListenAndServe()
}
