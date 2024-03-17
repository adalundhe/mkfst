// Package middleware provides login middlewares:
// - Auth: adds auth from session and populates user info
// - Trace: populates user info if token presented
// - AdminOnly: restrict access to admin users only
package auth

import (
	"crypto/subtle"
	"database/sql"
	"fmt"
	"net/http"
	"strings"

	"mkfst/auth/logger"
	"mkfst/auth/provider"
	"mkfst/auth/token"

	"github.com/gin-gonic/gin"
)

// Authenticator is top level auth object providing middlewares
type Authenticator struct {
	logger.L
	JWTService       TokenService
	Providers        []provider.Service
	Validator        token.Validator
	AdminPasswd      string
	BasicAuthChecker BasicAuthFunc
	RefreshCache     RefreshCache
}

// RefreshCache defines interface storing and retrieving refreshed tokens
type RefreshCache interface {
	Get(key interface{}) (value interface{}, ok bool)
	Set(key, value interface{})
}

// TokenService defines interface accessing tokens
type TokenService interface {
	Parse(tokenString string) (claims token.Claims, err error)
	Set(w http.ResponseWriter, claims token.Claims) (token.Claims, error)
	Get(r *http.Request) (claims token.Claims, token string, err error)
	IsExpired(claims token.Claims) bool
	Reset(w http.ResponseWriter)
}

// BasicAuthFunc type is an adapter to allow the use of ordinary functions as BasicAuth.
// The second return parameter `User` need for add user claims into context of request.
type BasicAuthFunc func(user, passwd string) (ok bool, userInfo token.User, err error)

// adminUser sets claims for an optional basic auth
var adminUser = token.User{
	ID:   "admin",
	Name: "admin",
	Attributes: map[string]interface{}{
		"admin": true,
	},
}

// Auth middleware adds auth from session and populates user info
func (a *Authenticator) Auth(ctx *gin.Context, db *sql.DB) (any, error) {
	return a.authorizeRequest(true)(ctx, db)
}

// Trace middleware doesn't require valid user but if user info presented populates info
func (a *Authenticator) Trace(ctx *gin.Context, db *sql.DB) (any, error) {
	return a.authorizeRequest(false)(ctx, db)
}

func (a *Authenticator) authorizeRequest(
	reqAuth bool,
) func(
	ctx *gin.Context,
	_ *sql.DB,
) (any, error) {

	return func(ctx *gin.Context, _ *sql.DB) (any, error) {
		// use admin user basic auth if enabled but ignore when BasicAuthChecker defined
		if a.BasicAuthChecker == nil && a.basicAdminUser(ctx.Request) {
			ctx.Request = token.SetUserInfo(ctx.Request, adminUser)
			ctx.Next()
			return nil, nil
		}

		// use custom basic auth if BasicAuthChecker defined
		if a.BasicAuthChecker != nil {
			if user, passwd, isBasicAuth := ctx.Request.BasicAuth(); isBasicAuth {
				ok, userInfo, err := a.BasicAuthChecker(user, passwd)
				if err != nil {
					return a.onError(
						ctx,
						fmt.Errorf("basic auth check failed: %w", err),
						reqAuth,
					)
				}
				if !ok {
					return a.onError(
						ctx,
						fmt.Errorf("credentials are wrong for basic auth: %w", err),
						reqAuth,
					)
				}
				ctx.Request = token.SetUserInfo(ctx.Request, userInfo) // pass user claims into context of incoming request
				ctx.Next()
				return nil, err
			}
		}

		claims, tkn, err := a.JWTService.Get(ctx.Request)
		if err != nil {
			return a.onError(
				ctx,
				fmt.Errorf("can't get token: %w", err),
				reqAuth,
			)
		}

		if claims.Handshake != nil { // handshake in token indicates special use cases, not for login
			return a.onError(
				ctx,
				fmt.Errorf("invalid kind of token"),
				reqAuth,
			)
		}

		if claims.User == nil {
			return a.onError(
				ctx,
				fmt.Errorf("no user info presented in the claim"),
				reqAuth,
			)
		}

		if claims.User != nil { // if uinfo in token populate it to context
			// validator passed by client and performs check on token or/and claims
			if a.Validator != nil && !a.Validator.Validate(tkn, claims) {
				res, err := a.onError(
					ctx,
					fmt.Errorf("user %s/%s blocked", claims.User.Name, claims.User.ID),
					reqAuth,
				)
				a.JWTService.Reset(ctx.Writer)
				return res, err
			}

			// check if user provider is allowed
			if !a.isProviderAllowed(claims.User.ID) {
				res, err := a.onError(
					ctx,
					fmt.Errorf("user %s/%s provider is not allowed", claims.User.Name, claims.User.ID),
					reqAuth,
				)
				a.JWTService.Reset(ctx.Writer)
				return res, err
			}

			if a.JWTService.IsExpired(claims) {
				if claims, err = a.refreshExpiredToken(
					ctx.Writer,
					claims,
					tkn,
				); err != nil {
					a.JWTService.Reset(ctx.Writer)
					return a.onError(
						ctx,
						fmt.Errorf("can't refresh token: %w", err),
						reqAuth,
					)
				}
			}

			ctx.Request = token.SetUserInfo(ctx.Request, *claims.User) // populate user info to request context
		}

		ctx.Next()
		return nil, nil
	}
}

func (a *Authenticator) onError(
	ctx *gin.Context,
	err error,
	reqAuth bool,
) (any, error) {
	if !reqAuth { // if no auth required allow to proceeded on error
		ctx.Next()
		return nil, nil
	}

	ctx.Error(err)
	ctx.AbortWithStatusJSON(401, gin.H{
		"message": "Unauthorized.",
	})

	return nil, err
}

// isProviderAllowed checks if user provider is allowed, user id looks like "provider_1234567890"
// this check is needed to reject users from providers what are used to be allowed but not anymore.
// Such users made token before the provider was disabled and should not be allowed to login anymore.
func (a *Authenticator) isProviderAllowed(userID string) bool {
	userProvider := strings.Split(userID, "_")[0]
	for _, p := range a.Providers {
		if p.Name() == userProvider {
			return true
		}
	}
	return false
}

// refreshExpiredToken makes a new token with passed claims
func (a *Authenticator) refreshExpiredToken(w http.ResponseWriter, claims token.Claims, tkn string) (token.Claims, error) {

	// cache refreshed claims for given token in order to eliminate multiple refreshes for concurrent requests
	if a.RefreshCache != nil {
		if c, ok := a.RefreshCache.Get(tkn); ok {
			// already in cache
			return c.(token.Claims), nil
		}
	}

	claims.ExpiresAt = 0                  // this will cause now+duration for refreshed token
	c, err := a.JWTService.Set(w, claims) // Set changes token
	if err != nil {
		return token.Claims{}, err
	}

	if a.RefreshCache != nil {
		a.RefreshCache.Set(tkn, c)
	}

	a.Logf("[DEBUG] token refreshed for %+v", claims.User)
	return c, nil
}

// AdminOnly middleware allows access for admins only
// this handler internally wrapped with auth(true) to avoid situation if AdminOnly defined without prior Auth
func (a *Authenticator) AdminOnly() func(ctx *gin.Context, db *sql.DB) (any, error) {

	return func(ctx *gin.Context, db *sql.DB) (any, error) {
		_, err := a.authorizeRequest(true)(ctx, db)
		if err != nil {
			return a.onError(ctx, err, true)
		}

		user, err := token.GetUserInfo(ctx.Request)
		if err != nil {
			return a.onError(ctx, err, true)
		}

		if !user.IsAdmin() {
			return a.onError(ctx, err, true)
		}

		ctx.Next()

		return nil, nil
	}
}

// basic auth for admin user
func (a *Authenticator) basicAdminUser(r *http.Request) bool {

	if a.AdminPasswd == "" {
		return false
	}

	user, passwd, ok := r.BasicAuth()
	if !ok {
		return false
	}

	// using ConstantTimeCompare to avoid timing attack
	if user != "admin" || subtle.ConstantTimeCompare([]byte(passwd), []byte(a.AdminPasswd)) != 1 {
		a.Logf("[WARN] admin basic auth failed, user/passwd mismatch, %s:%s", user, passwd)
		return false
	}

	return true
}

// RBAC middleware allows role based control for routes
// this handler internally wrapped with auth(true) to avoid situation if RBAC defined without prior Auth
func (a *Authenticator) RBAC(roles ...string) func(
	ctx *gin.Context,
	db *sql.DB,
) (any, error) {

	return func(ctx *gin.Context, db *sql.DB) (any, error) {

		_, err := a.authorizeRequest(true)(ctx, db)
		if err != nil {
			return a.onError(ctx, err, true)
		}

		user, err := token.GetUserInfo(ctx.Request)
		if err != nil {
			return a.onError(ctx, err, true)
		}

		var matched bool
		for _, role := range roles {
			if strings.EqualFold(role, user.Role) {
				matched = true
				break
			}
		}
		if !matched {
			return a.onError(ctx, err, true)
		}

		ctx.Next()
		return nil, nil
	}
}
