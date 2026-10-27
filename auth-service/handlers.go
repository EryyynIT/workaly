package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	pb "github.com/EryyynIT/workaly/auth-service/pb/userpb"
	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

type application struct {
	cfg config
	// db is nil only in unit tests, which stub every storage call — that is
	// why handlers branch on it. shortcut: removing these branches requires a
	// test database (sqlmock or testcontainers); deferred until module
	// downloads are reliable.
	db      *sql.DB
	users   pb.UserServiceClient
	health  healthpb.HealthClient
	mail    mailSender
	limiter *rateLimiter
}

type registerRequest struct {
	Name     string `json:"name" binding:"required,max=50"`
	Username string `json:"username" binding:"required,max=255"`
	Email    string `json:"email" binding:"required,email,max=255"`
	Password string `json:"password" binding:"required,min=12"`
}

type loginRequest struct {
	Email    string `json:"email" binding:"required,email,max=255"`
	Password string `json:"password" binding:"required"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token" binding:"required"`
}

type forgotRequest struct {
	Email string `json:"email" binding:"required,email,max=255"`
}

type resetRequest struct {
	Token    string `json:"token" binding:"required"`
	Password string `json:"password" binding:"required,min=12"`
}

type verifyRequest struct {
	Token string `json:"token" binding:"required"`
}

func (app *application) router() *gin.Engine {
	router := gin.New()
	// Gin's default recovery dumps request headers, which can include session cookies.
	router.Use(gin.CustomRecoveryWithWriter(io.Discard, func(c *gin.Context, _ any) {
		slog.Error("request panic recovered")
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "internal service error"})
	}))
	// Do not trust caller-supplied proxy headers before a real gateway is configured.
	_ = router.SetTrustedProxies(nil)
	router.GET("/healthz", func(c *gin.Context) { c.Status(http.StatusOK) })
	router.GET("/readyz", app.ready)
	router.POST("/register", app.jsonRequest, app.register)
	router.POST("/login", app.jsonRequest, app.login)
	router.POST("/refresh", app.jsonRequest, app.refresh)
	router.POST("/logout", app.jsonRequest, app.logout)
	router.POST("/forgot-password", app.jsonRequest, app.forgotPassword)
	router.POST("/reset-password", app.jsonRequest, app.resetPassword)
	router.POST("/verify-email", app.jsonRequest, app.verifyEmail)
	router.GET("/sessions", app.listSessions)
	router.POST("/logout-all", app.jsonRequest, app.logoutAll)
	router.GET("/users/:user_id", app.publicProfile)
	return router
}

func (app *application) jsonRequest(c *gin.Context) {
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != "application/json" {
		c.AbortWithStatusJSON(http.StatusUnsupportedMediaType, gin.H{"error": "application/json required"})
		return
	}
	if origin := c.GetHeader("Origin"); (origin != "" && origin != app.cfg.PublicOrigin) || c.GetHeader("Sec-Fetch-Site") == "cross-site" {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "origin not allowed"})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 8<<10)
	c.Next()
}

func (app *application) register(c *gin.Context) {
	var req registerRequest
	if err := bindJSON(c, &req); err != nil || !validText(req.Name) || !validText(req.Username) || len(req.Password) > 1024 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid registration fields"})
		return
	}
	if !app.limiter.Allow("register:"+req.Email, 3, time.Minute) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many attempts"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()

	passwordHash, err := hashPassword(req.Password)
	if err != nil {
		slog.Error("password hash failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal service error"})
		return
	}
	accountID, err := newUUID()
	if err != nil {
		slog.Error("uuid generation failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal service error"})
		return
	}

	if app.db != nil {
		tx, err := app.db.BeginTx(ctx, nil)
		if err != nil {
			writeDBError(c, err)
			return
		}
		defer func() { _ = tx.Rollback() }()

		_, err = tx.ExecContext(ctx,
			"INSERT INTO accounts (id, email, password_hash) VALUES ($1, $2, $3)",
			accountID, req.Email, passwordHash)
		if err != nil {
			if isDuplicateKey(err) {
				// Do not confirm that the email is registered; same wording as
				// a successful registration.
				c.JSON(http.StatusOK, gin.H{
					"message": "if the email is not registered, a verification link has been sent",
				})
				return
			}
			writeDBError(c, err)
			return
		}
		_, err = tx.ExecContext(ctx,
			"INSERT INTO pending_profiles (account_id, name, username) VALUES ($1, $2, $3)",
			accountID, req.Name, req.Username)
		if err != nil {
			writeDBError(c, err)
			return
		}
		if err := tx.Commit(); err != nil {
			writeDBError(c, err)
			return
		}
	}

	if err := app.deliverProfileSync(ctx, accountID, req.Name, req.Username); err != nil {
		// The username is taken by someone else: a half-created account would
		// never receive a profile and would keep this email from registering
		// with a free username. No tokens or emails were issued yet.
		if status.Code(err) == codes.AlreadyExists && app.db != nil {
			if _, err := app.db.ExecContext(ctx,
				"DELETE FROM accounts WHERE id = $1", accountID); err != nil {
				slog.Error("register cleanup after username conflict failed", "error", err)
			}
		}
		if app.db != nil {
			_, _ = app.db.ExecContext(ctx,
				"UPDATE pending_profiles SET last_sent_at = CURRENT_TIMESTAMP WHERE account_id = $1", accountID)
		}
		writeRPCError(c, err)
		return
	}
	if app.db != nil {
		app.sendVerificationEmail(ctx, accountID, req.Email)
		app.audit(ctx, accountID, "register", nil)
	}

	tokens, err := app.createSession(ctx, accountID)
	if err != nil {
		writeDBError(c, err)
		return
	}
	app.issueTokens(c, accountID, tokens)
}

func (app *application) login(c *gin.Context) {
	var req loginRequest
	if err := bindJSON(c, &req); err != nil || len(req.Password) > 1024 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid login fields"})
		return
	}
	if !app.limiter.Allow("login:"+req.Email, 5, time.Minute) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many attempts"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()

	var accountID, storedHash string
	var verifiedAt *time.Time
	if app.db != nil {
		err := app.db.QueryRowContext(ctx,
			"SELECT id, password_hash, email_verified_at FROM accounts WHERE lower(email) = lower($1)",
			req.Email).Scan(&accountID, &storedHash, &verifiedAt)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			writeDBError(c, err)
			return
		}
		if err == nil {
			hash := storedHash
			if !verifyPassword(req.Password, hash) {
				app.audit(ctx, accountID, "login_failed", map[string]string{"email": req.Email})
				c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
				return
			}
			if verifiedAt == nil {
				// Resending is rate-limited per account to avoid mail flooding.
				if app.limiter.Allow("verify:"+accountID, 1, time.Hour) {
					app.sendVerificationEmail(ctx, accountID, req.Email)
				}
			}
		} else {
			// Unknown account: verify against dummy hash to equalize timing.
			verifyPassword(req.Password, dummyHash)
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
			return
		}
	} else {
		// No DB in unit tests: simulate a successful login.
		accountID = "00000000-0000-0000-0000-000000000001"
	}

	app.audit(ctx, accountID, "login", nil)
	tokens, err := app.createSession(ctx, accountID)
	if err != nil {
		writeDBError(c, err)
		return
	}
	app.issueTokens(c, accountID, tokens)
}

func (app *application) refresh(c *gin.Context) {
	var req refreshRequest
	if err := bindJSON(c, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid refresh fields"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()

	if app.db == nil {
		c.JSON(http.StatusOK, gin.H{"message": "success"})
		return
	}

	hash := hashToken(req.RefreshToken)
	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		writeDBError(c, err)
		return
	}
	defer func() { _ = tx.Rollback() }()

	// Row lock: a concurrent refresh with the same token waits here, then
	// sees the rotated-out row and triggers reuse detection instead of
	// creating a second live session.
	var sessionID, accountID string
	var expiresAt time.Time
	var revokedAt *time.Time
	var replacedBy *string
	err = tx.QueryRowContext(ctx,
		"SELECT id, account_id, expires_at, revoked_at, replaced_by FROM sessions WHERE refresh_token_hash = $1 FOR UPDATE",
		hash).Scan(&sessionID, &accountID, &expiresAt, &revokedAt, &replacedBy)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid session"})
			return
		}
		writeDBError(c, err)
		return
	}
	if revokedAt != nil || time.Now().After(expiresAt) {
		// A rotated-out token being replayed means the token leaked: revoke
		// the whole session family. The update must run in this transaction —
		// the row lock is held here, a second connection would wait on itself.
		if replacedBy != nil {
			if _, err := tx.ExecContext(ctx,
				"UPDATE sessions SET revoked_at = CURRENT_TIMESTAMP WHERE account_id = $1 AND revoked_at IS NULL", accountID); err != nil {
				slog.Warn("session family revocation failed", "account_id", accountID, "error", err)
			}
			if err := tx.Commit(); err != nil {
				slog.Warn("session family revocation commit failed", "error", err)
			}
			app.audit(ctx, accountID, "refresh_reuse_detected", nil)
		}
		c.JSON(http.StatusUnauthorized, gin.H{"error": "session expired"})
		return
	}

	newPlain, newHash, err := newRefreshToken()
	if err != nil {
		writeDBError(c, err)
		return
	}
	newSessionID, err := newUUID()
	if err != nil {
		writeDBError(c, err)
		return
	}

	_, err = tx.ExecContext(ctx,
		"INSERT INTO sessions (id, account_id, refresh_token_hash, expires_at) VALUES ($1, $2, $3, $4)",
		newSessionID, accountID, newHash, time.Now().Add(refreshTokenTTL))
	if err != nil {
		writeDBError(c, err)
		return
	}
	_, err = tx.ExecContext(ctx,
		"UPDATE sessions SET revoked_at = CURRENT_TIMESTAMP, previous_token_hash = $1, replaced_by = $2 WHERE id = $3",
		newHash, newSessionID, sessionID)
	if err != nil {
		writeDBError(c, err)
		return
	}
	if err := tx.Commit(); err != nil {
		writeDBError(c, err)
		return
	}

	app.audit(ctx, accountID, "refresh", nil)
	app.issueTokens(c, accountID, sessionTokens{RefreshToken: newPlain, RefreshExpires: time.Now().Add(refreshTokenTTL)})
}

func (app *application) logout(c *gin.Context) {
	var req refreshRequest
	if err := bindJSON(c, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid logout fields"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()

	if app.db != nil {
		hash := hashToken(req.RefreshToken)
		var accountID string
		err := app.db.QueryRowContext(ctx,
			"UPDATE sessions SET revoked_at = CURRENT_TIMESTAMP WHERE refresh_token_hash = $1 AND revoked_at IS NULL RETURNING account_id",
			hash).Scan(&accountID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			writeDBError(c, err)
			return
		}
		if accountID != "" {
			app.audit(ctx, accountID, "logout", nil)
		}
	}
	c.SetCookie("token", "", -1, "/", "", app.cfg.CookieSecure, true)
	c.JSON(http.StatusOK, gin.H{"message": "logged out"})
}

func (app *application) forgotPassword(c *gin.Context) {
	var req forgotRequest
	if err := bindJSON(c, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request fields"})
		return
	}
	if !app.limiter.Allow("forgot:"+req.Email, 3, time.Hour) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many attempts"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()

	if app.db != nil {
		var accountID string
		err := app.db.QueryRowContext(ctx,
			"SELECT id FROM accounts WHERE lower(email) = lower($1)", req.Email).Scan(&accountID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			writeDBError(c, err)
			return
		}
		if err == nil {
			token, hash, err := newRefreshToken()
			if err != nil {
				writeDBError(c, err)
				return
			}
			app.pruneUsedTokens(ctx)
			_, err = app.db.ExecContext(ctx,
				"INSERT INTO action_tokens (account_id, kind, token_hash, expires_at) VALUES ($1, 'password_reset', $2, $3)",
				accountID, hash, time.Now().Add(time.Hour))
			if err != nil {
				writeDBError(c, err)
				return
			}
			_ = app.mail.Send(ctx, req.Email, "Workaly password reset",
				"Reset token (valid 1 hour): "+token+"\nIf you did not request this, ignore this message.")
			app.audit(ctx, accountID, "forgot_password", nil)
		}
	}
	// Same response whether the account exists or not.
	c.JSON(http.StatusOK, gin.H{"message": "if the account exists, a reset link has been sent"})
}

func (app *application) resetPassword(c *gin.Context) {
	var req resetRequest
	if err := bindJSON(c, &req); err != nil || len(req.Password) > 1024 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request fields"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()

	if app.db == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
		return
	}

	hash := hashToken(req.Token)
	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		writeDBError(c, err)
		return
	}
	defer func() { _ = tx.Rollback() }()

	var accountID string
	err = tx.QueryRowContext(ctx,
		"UPDATE action_tokens SET used_at = CURRENT_TIMESTAMP WHERE token_hash = $1 AND kind = 'password_reset' AND used_at IS NULL AND expires_at > CURRENT_TIMESTAMP RETURNING account_id",
		hash).Scan(&accountID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			return
		}
		writeDBError(c, err)
		return
	}

	passwordHash, err := hashPassword(req.Password)
	if err != nil {
		writeDBError(c, err)
		return
	}
	// One transaction: a consumed token must never leave the old password active.
	_, err = tx.ExecContext(ctx,
		"UPDATE accounts SET password_hash = $1 WHERE id = $2", passwordHash, accountID)
	if err != nil {
		writeDBError(c, err)
		return
	}
	_, err = tx.ExecContext(ctx,
		"UPDATE sessions SET revoked_at = CURRENT_TIMESTAMP WHERE account_id = $1", accountID)
	if err != nil {
		writeDBError(c, err)
		return
	}
	if err := tx.Commit(); err != nil {
		writeDBError(c, err)
		return
	}
	app.audit(ctx, accountID, "reset_password", nil)
	c.JSON(http.StatusOK, gin.H{"message": "password updated"})
}

func (app *application) verifyEmail(c *gin.Context) {
	var req verifyRequest
	if err := bindJSON(c, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request fields"})
		return
	}
	if app.db == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()

	hash := hashToken(req.Token)
	tx, err := app.db.BeginTx(ctx, nil)
	if err != nil {
		writeDBError(c, err)
		return
	}
	defer func() { _ = tx.Rollback() }()

	var accountID string
	err = tx.QueryRowContext(ctx,
		"UPDATE action_tokens SET used_at = CURRENT_TIMESTAMP WHERE token_hash = $1 AND kind = 'verify_email' AND used_at IS NULL AND expires_at > CURRENT_TIMESTAMP RETURNING account_id",
		hash).Scan(&accountID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			return
		}
		writeDBError(c, err)
		return
	}
	// One transaction: a consumed token must never leave the email unverified.
	if _, err = tx.ExecContext(ctx,
		"UPDATE accounts SET email_verified_at = CURRENT_TIMESTAMP WHERE id = $1", accountID); err != nil {
		writeDBError(c, err)
		return
	}
	if err := tx.Commit(); err != nil {
		writeDBError(c, err)
		return
	}
	app.audit(ctx, accountID, "verify_email", nil)
	c.JSON(http.StatusOK, gin.H{"message": "email verified"})
}

func (app *application) listSessions(c *gin.Context) {
	if app.db == nil {
		c.JSON(http.StatusOK, gin.H{"sessions": []any{}})
		return
	}
	accountID, err := app.requireAuth(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()

	rows, err := app.db.QueryContext(ctx,
		"SELECT id, created_at, expires_at FROM sessions WHERE account_id = $1 AND revoked_at IS NULL AND expires_at > CURRENT_TIMESTAMP ORDER BY created_at DESC",
		accountID)
	if err != nil {
		writeDBError(c, err)
		return
	}
	defer rows.Close()

	type sessionInfo struct {
		ID        string    `json:"id"`
		CreatedAt time.Time `json:"created_at"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	sessions := []sessionInfo{}
	for rows.Next() {
		var s sessionInfo
		if err := rows.Scan(&s.ID, &s.CreatedAt, &s.ExpiresAt); err != nil {
			writeDBError(c, err)
			return
		}
		sessions = append(sessions, s)
	}
	// A network drop mid-iteration ends the loop silently without this check,
	// which would return a partial session list as a success.
	if err := rows.Err(); err != nil {
		writeDBError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"sessions": sessions})
}

func (app *application) logoutAll(c *gin.Context) {
	accountID, err := app.requireAuth(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()

	if app.db != nil {
		app.revokeAllSessions(ctx, accountID)
		app.audit(ctx, accountID, "logout_all", nil)
	}
	c.SetCookie("token", "", -1, "/", "", app.cfg.CookieSecure, true)
	c.JSON(http.StatusOK, gin.H{"message": "all sessions revoked"})
}

// publicProfile serves the public view of a profile: name and username only.
// Any client may read it without a session; user_id in the path is the address
// of public data, not an identity claim, so nothing is authorized by it.
// Unlike the auth flows, a missing profile is a normal 404, not a credentials
// error — that is why this endpoint keeps its own RPC error mapping.
func (app *application) publicProfile(c *gin.Context) {
	if !app.limiter.Allow("profile:"+c.ClientIP(), 60, time.Minute) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many attempts"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), requestTimeout)
	defer cancel()

	userID := c.Param("user_id")
	profile, err := app.users.GetPublicProfile(ctx, &pb.GetPublicProfileRequest{UserId: userID})
	if err != nil {
		switch status.Code(err) {
		case codes.InvalidArgument:
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user id"})
		case codes.NotFound:
			c.JSON(http.StatusNotFound, gin.H{"error": "profile not found"})
		case codes.Unavailable:
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "account service unavailable"})
		case codes.DeadlineExceeded:
			c.JSON(http.StatusGatewayTimeout, gin.H{"error": "operation timed out; result may be unknown"})
		default:
			// Only the code is logged: upstream errors can contain database details.
			slog.Warn("profile request failed", "grpc_code", status.Code(err).String())
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal service error"})
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"user_id": userID, "name": profile.Name, "username": profile.Username})
}

// requireAuth validates the access token cookie and returns the account UUID.
func (app *application) requireAuth(c *gin.Context) (string, error) {
	cookie, err := c.Cookie("token")
	if err != nil {
		return "", errors.New("no token")
	}
	claims := jwt.RegisteredClaims{}
	_, err = jwt.ParseWithClaims(cookie, &claims, func(*jwt.Token) (any, error) {
		return app.cfg.JWTSecret, nil
	}, jwt.WithIssuer("workaly-auth"), jwt.WithAudience("workaly"))
	if err != nil {
		return "", err
	}
	return claims.Subject, nil
}

func (app *application) createSession(ctx context.Context, accountID string) (sessionTokens, error) {
	if app.db == nil {
		return sessionTokens{RefreshToken: "test-refresh", RefreshExpires: time.Now().Add(refreshTokenTTL)}, nil
	}
	plain, hash, err := newRefreshToken()
	if err != nil {
		return sessionTokens{}, err
	}
	sessionID, err := newUUID()
	if err != nil {
		return sessionTokens{}, err
	}
	_, err = app.db.ExecContext(ctx,
		"INSERT INTO sessions (id, account_id, refresh_token_hash, expires_at) VALUES ($1, $2, $3, $4)",
		sessionID, accountID, hash, time.Now().Add(refreshTokenTTL))
	if err != nil {
		return sessionTokens{}, err
	}
	return sessionTokens{RefreshToken: plain, RefreshExpires: time.Now().Add(refreshTokenTTL)}, nil
}

func (app *application) revokeAllSessions(ctx context.Context, accountID string) {
	if app.db == nil {
		return
	}
	_, _ = app.db.ExecContext(ctx,
		"UPDATE sessions SET revoked_at = CURRENT_TIMESTAMP WHERE account_id = $1", accountID)
}

type sessionTokens struct {
	RefreshToken   string
	RefreshExpires time.Time
}

func (app *application) issueTokens(c *gin.Context, accountID string, tokens sessionTokens) {
	now := time.Now().UTC()
	claims := jwt.RegisteredClaims{
		Issuer:    "workaly-auth",
		Audience:  jwt.ClaimStrings{"workaly"},
		Subject:   accountID,
		ExpiresAt: jwt.NewNumericDate(now.Add(accessTokenTTL)),
		IssuedAt:  jwt.NewNumericDate(now),
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(app.cfg.JWTSecret)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not create session"})
		return
	}
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie("token", token, int(accessTokenTTL.Seconds()), "/", "", app.cfg.CookieSecure, true)
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{
		"message":         "success",
		"user_id":         accountID,
		"refresh_token":   tokens.RefreshToken,
		"refresh_expires": tokens.RefreshExpires.Format(time.RFC3339),
	})
}

func (app *application) deliverProfileSync(ctx context.Context, accountID, name, username string) error {
	_, err := app.users.CreateProfile(ctx, &pb.CreateProfileRequest{
		UserId: accountID, Name: name, Username: username,
	})
	if err != nil {
		return err
	}
	if app.db != nil {
		_, _ = app.db.ExecContext(ctx,
			"DELETE FROM pending_profiles WHERE account_id = $1", accountID)
	}
	return nil
}

// pruneUsedTokens deletes consumed and expired action tokens so the table
// does not grow without bound. Runs on the rare mail paths, not on a timer.
func (app *application) pruneUsedTokens(ctx context.Context) {
	if app.db == nil {
		return
	}
	if _, err := app.db.ExecContext(ctx,
		"DELETE FROM action_tokens WHERE used_at IS NOT NULL OR expires_at < CURRENT_TIMESTAMP"); err != nil {
		slog.Warn("action token cleanup failed", "error", err)
	}
}

func (app *application) sendVerificationEmail(ctx context.Context, accountID, email string) {
	if app.db == nil {
		return
	}
	app.pruneUsedTokens(ctx)
	token, hash, err := newRefreshToken()
	if err != nil {
		return
	}
	_, err = app.db.ExecContext(ctx,
		"INSERT INTO action_tokens (account_id, kind, token_hash, expires_at) VALUES ($1, 'verify_email', $2, $3)",
		accountID, hash, time.Now().Add(24*time.Hour))
	if err != nil {
		// Without the stored token the link would 401: say so in the log.
		slog.Warn("verify token not stored; mail skipped", "account_id", accountID, "error", err)
		return
	}
	_ = app.mail.Send(ctx, email, "Workaly email verification",
		"Verification token (valid 24 hours): "+token+"\nIf you did not register, ignore this message.")
}

func (app *application) audit(ctx context.Context, accountID, action string, detail map[string]string) {
	if app.db == nil {
		return
	}
	if accountID == "" {
		accountID = "00000000-0000-0000-0000-000000000000"
	}
	detailJSON := "{}"
	if len(detail) > 0 {
		b, _ := json.Marshal(detail)
		detailJSON = string(b)
	}
	_, _ = app.db.ExecContext(ctx,
		"INSERT INTO audit_log (account_id, action, detail) VALUES ($1, $2, $3)",
		accountID, action, detailJSON)
}

func validText(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && strings.IndexFunc(value, unicode.IsControl) == -1
}

// hasInvalidUTF8JSON rejects the input that encoding/json silently replaces
// with U+FFFD: invalid UTF-8 and unpaired \uD800-\uDFFF surrogate escapes.
func hasInvalidUTF8JSON(body []byte) bool {
	if !utf8.Valid(body) {
		return true
	}
	inString := false
	for i := 0; i < len(body); i++ {
		if !inString {
			if body[i] == '"' {
				inString = true
			}
			continue
		}
		if body[i] == '"' {
			inString = false
			continue
		}
		if body[i] != '\\' {
			continue
		}
		if i+5 >= len(body) || body[i+1] != 'u' {
			i++ // single-character escape
			continue
		}
		value, ok := hex4(body[i+2 : i+6])
		if !ok {
			i++
			continue // invalid JSON; decoding reports it
		}
		if 0xD800 <= value && value <= 0xDBFF {
			if i+11 < len(body) && body[i+6] == '\\' && body[i+7] == 'u' {
				if low, ok := hex4(body[i+8 : i+12]); ok && 0xDC00 <= low && low <= 0xDFFF {
					i += 11
					continue
				}
			}
			return true
		}
		if 0xDC00 <= value && value <= 0xDFFF {
			return true
		}
		i += 5
	}
	return false
}

func hex4(value []byte) (int, bool) {
	result := 0
	for _, c := range value {
		result <<= 4
		switch {
		case '0' <= c && c <= '9':
			result += int(c - '0')
		case 'a' <= c && c <= 'f':
			result += int(c-'a') + 10
		case 'A' <= c && c <= 'F':
			result += int(c-'A') + 10
		default:
			return 0, false
		}
	}
	return result, true
}

func bindJSON(c *gin.Context, value any) error {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return err
	}
	// json.Decoder replaces invalid UTF-8 and unpaired surrogates with U+FFFD.
	// Never turn supplied credentials into different characters silently.
	if hasInvalidUTF8JSON(body) {
		return io.ErrUnexpectedEOF
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(value); err != nil {
		return err
	}
	// Require one document so trailing JSON cannot bypass validation.
	if err := decoder.Decode(new(any)); err != io.EOF {
		return io.ErrUnexpectedEOF
	}
	return binding.Validator.ValidateStruct(value)
}

// isDuplicateKey reports a PostgreSQL unique-violation error (SQLSTATE 23505).
func isDuplicateKey(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func writeDBError(c *gin.Context, err error) {
	msg := err.Error()
	// Connection-level failures first: nothing was executed, so a retry is
	// safe. A connect timeout can also wrap context.DeadlineExceeded, which
	// is why this check must come before the deadline mapping.
	if strings.Contains(msg, "failed to connect") || strings.Contains(msg, "connection refused") || strings.Contains(msg, "connection reset") || strings.Contains(msg, "no such host") {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "account service unavailable"})
		return
	}
	var pgErr *pgconn.PgError
	// 57014: statement timeout set by the server (statement_timeout).
	if errors.As(err, &pgErr) && pgErr.Code == "57014" {
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "operation timed out; result may be unknown"})
		return
	}
	// A query-side deadline: the statement may or may not have been applied.
	if errors.Is(err, context.DeadlineExceeded) {
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "operation timed out; result may be unknown"})
		return
	}
	if strings.Contains(msg, "timeout") {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "account service unavailable"})
		return
	}
	slog.Error("database error", "error", err)
	c.JSON(http.StatusInternalServerError, gin.H{"error": "internal service error"})
}

func writeRPCError(c *gin.Context, err error) {
	code, message := http.StatusInternalServerError, "internal service error"
	switch status.Code(err) {
	case codes.AlreadyExists:
		code, message = http.StatusConflict, "username already exists"
	case codes.Unauthenticated, codes.NotFound:
		code, message = http.StatusUnauthorized, "invalid credentials"
	case codes.InvalidArgument:
		code, message = http.StatusBadRequest, "invalid request fields"
	case codes.ResourceExhausted:
		code, message = http.StatusTooManyRequests, "service busy; try again later"
	case codes.Unavailable:
		code, message = http.StatusServiceUnavailable, "account service unavailable"
	case codes.DeadlineExceeded:
		code, message = http.StatusGatewayTimeout, "operation timed out; result may be unknown"
	}
	// Only the code is logged: upstream errors can contain credentials or database details.
	slog.Warn("account request failed", "grpc_code", status.Code(err).String())
	c.JSON(code, gin.H{"error": message})
}

func (app *application) ready(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), time.Second)
	defer cancel()
	res, err := app.health.Check(ctx, &healthpb.HealthCheckRequest{Service: "user.UserService"})
	if err != nil || res == nil || res.Status != healthpb.HealthCheckResponse_SERVING {
		c.Status(http.StatusServiceUnavailable)
		return
	}
	if app.db != nil {
		if err := app.db.PingContext(ctx); err != nil {
			c.Status(http.StatusServiceUnavailable)
			return
		}
	}
	c.Status(http.StatusOK)
}
