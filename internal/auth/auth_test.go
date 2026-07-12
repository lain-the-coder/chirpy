package auth

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMakeAndValidateJWT(t *testing.T) {
	userID := uuid.New()
	secret := "mysecret"

	tokenString, err := MakeJWT(userID, secret, time.Hour)
	if err != nil {
		t.Fatalf("MakeJWT returned an error: %v", err)
	}

	gotUserID, err := ValidateJWT(tokenString, secret)
	if err != nil {
		t.Fatalf("ValidateJWT returned an error: %v", err)
	}

	if gotUserID != userID {
		t.Errorf("expected user ID %v, got %v", userID, gotUserID)
	}
}

func TestValidateJWTExpired(t *testing.T) {
	userID := uuid.New()
	secret := "mysecret"

	// expiresIn is negative, so the token is already expired the moment it's created
	tokenString, err := MakeJWT(userID, secret, -time.Hour)
	if err != nil {
		t.Fatalf("MakeJWT returned an error: %v", err)
	}

	_, err = ValidateJWT(tokenString, secret)
	if err == nil {
		t.Errorf("expected an error for expired token, got nil")
	}
}

func TestValidateJWTWrongSecret(t *testing.T) {
	userID := uuid.New()
	secret := "mysecret"
	wrongSecret := "wrongsecret"

	tokenString, err := MakeJWT(userID, secret, time.Hour)
	if err != nil {
		t.Fatalf("MakeJWT returned an error: %v", err)
	}

	_, err = ValidateJWT(tokenString, wrongSecret)
	if err == nil {
		t.Errorf("expected an error for wrong secret, got nil")
	}
}

func TestGetBearerToken(t *testing.T) {
	headers := http.Header{}
	headers.Set("Authorization", "Bearer sometoken123")

	token, err := GetBearerToken(headers)
	if err != nil {
		t.Fatalf("GetBearerToken returned an error: %v", err)
	}
	if token != "sometoken123" {
		t.Errorf("expected token %q, got %q", "sometoken123", token)
	}
}

func TestGetBearerTokenMissingHeader(t *testing.T) {
	headers := http.Header{}

	_, err := GetBearerToken(headers)
	if err == nil {
		t.Errorf("expected an error for missing Authorization header, got nil")
	}
}

func TestGetBearerTokenMalformed(t *testing.T) {
	headers := http.Header{}
	headers.Set("Authorization", "sometoken123")

	_, err := GetBearerToken(headers)
	if err == nil {
		t.Errorf("expected an error for malformed Authorization header, got nil")
	}
}
