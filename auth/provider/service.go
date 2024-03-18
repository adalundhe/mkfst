package provider

import (
	"crypto/rand"
	"crypto/sha1"
	"errors"
	"fmt"
	"net/http"

	"mkfst/auth/token"

	"github.com/gin-gonic/gin"
)

const (
	urlLoginSuffix    = "/login"
	urlCallbackSuffix = "/callback"
	urlLogoutSuffix   = "/logout"
)

// Service represents oauth2 provider. Adds Handler method multiplexing login, auth and logout requests
type Service struct {
	Provider
}

// NewService makes service for given provider
func NewService(p Provider) Service {
	return Service{Provider: p}
}

// AvatarSaver defines minimal interface to save avatar
type AvatarSaver interface {
	Put(u token.User, client *http.Client) (avatarURL string, err error)
}

// TokenService defines interface accessing tokens
type TokenService interface {
	Parse(tokenString string) (claims token.Claims, err error)
	Set(w http.ResponseWriter, claims token.Claims) (token.Claims, error)
	Get(r *http.Request) (claims token.Claims, token string, err error)
	Reset(w http.ResponseWriter)
}

// Provider defines interface for auth handler
type Provider interface {
	Name() string
	LoginHandler(w http.ResponseWriter, r *http.Request)
	AuthHandler(w http.ResponseWriter, r *http.Request)
	LogoutHandler(w http.ResponseWriter, r *http.Request)
}

// Handler returns auth routes for given provider
func (p Service) Handler(ctx *gin.Context) (gin.H, error) {

	if ctx.Request.Method != http.MethodGet && ctx.Request.Method != http.MethodPost {

		ctx.AbortWithStatus(http.StatusMethodNotAllowed)
		errMsg := "method not allowed"
		return gin.H{
			"error": errMsg,
		}, errors.New(errMsg)
	}

	action := ctx.Query("action")

	switch action {
	case "login":
		p.LoginHandler(ctx.Writer, ctx.Request)
		return nil, nil

	case "callback":
		p.AuthHandler(ctx.Writer, ctx.Request)
		return nil, nil

	case "logout":
		p.LogoutHandler(ctx.Writer, ctx.Request)
		return nil, nil

	default:
		ctx.AbortWithStatus(http.StatusNotFound)
		errMsg := "invalid action"
		return gin.H{
			"error": errMsg,
		}, errors.New(errMsg)
	}

}

// setAvatar saves avatar and puts proxied URL to u.Picture
func setAvatar(ava AvatarSaver, u token.User, client *http.Client) (token.User, error) {
	if ava != nil {
		avatarURL, e := ava.Put(u, client)
		if e != nil {
			return u, fmt.Errorf("failed to save avatar for: %w", e)
		}
		u.Picture = avatarURL
		return u, nil
	}
	return u, nil // empty AvatarSaver ok, just skipped
}

func randToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("can't get random: %w", err)
	}
	s := sha1.New()
	if _, err := s.Write(b); err != nil {
		return "", fmt.Errorf("can't write randoms to sha1: %w", err)
	}
	return fmt.Sprintf("%x", s.Sum(nil)), nil
}
