package controllers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cuadra/cuadra-core/src/shared/auth"
	"github.com/cuadra/cuadra-core/src/shared/middleware"
	"github.com/cuadra/cuadra-core/src/shared/r2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func TestPhotoSigningStaysInsideAuthenticatedGym(t *testing.T) {
	// Presigning is local cryptography: these synthetic credentials never
	// perform a network request or upload an object to a provider.
	client, err := r2.NewClient(r2.Config{AccountID: "isolated-test", AccessKey: "synthetic", SecretKey: "synthetic", Bucket: "private-test"})
	if err != nil {
		t.Fatal(err)
	}
	tokens := auth.NewJWTService("isolated-file-scope-test")
	gym, other, member := uuid.New(), uuid.New(), uuid.New()
	token, _ := tokens.GenerateAccessToken(uuid.New(), gym, "owner")
	ctrl := AuthController{Tokens: tokens, R2: client}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(middleware.AuthMiddleware(tokens))
	router.POST("/download", ctrl.handleUploadPresignGet)
	router.POST("/upload", ctrl.handleUploadPresign)
	request := func(path string, value any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(value)
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		router.ServeHTTP(out, req)
		return out
	}
	for _, test := range []struct {
		key    string
		status int
	}{
		{"gyms/" + other.String() + "/members/" + member.String() + ".png", 403},
		{"gyms/" + gym.String() + "/../" + other.String() + "/photo.png", 400},
		{"gyms/" + gym.String() + "/members/" + member.String() + ".png", 200},
	} {
		if out := request("/download", map[string]string{"object_key": test.key}); out.Code != test.status {
			t.Fatalf("download: status %d want %d", out.Code, test.status)
		}
	}
	out := request("/upload", map[string]string{"kind": "member_photo", "member_id": member.String(), "gym_id": other.String(), "ext": "png", "content_type": "image/png"})
	if out.Code != 200 || !strings.Contains(out.Body.String(), "gyms/"+gym.String()+"/members/") || strings.Contains(out.Body.String(), other.String()) {
		t.Fatal("upload signing escaped authenticated gym")
	}
}
