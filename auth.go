package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/labstack/echo/v5"
)

func parseAuthUsers(raw string) (map[string][32]byte, error) {
	var entries []string
	if err := json.Unmarshal([]byte(raw), &entries); err != nil || len(entries) == 0 {
		return nil, errors.New(`AUTH_USERS must be a nonempty JSON array of "username:password" strings`)
	}
	users := make(map[string][32]byte, len(entries))
	for i, entry := range entries {
		username, password, ok := strings.Cut(entry, ":")
		if !ok || username == "" || username != strings.TrimSpace(username) || strings.ContainsAny(username, "\r\n\t") || strings.TrimSpace(password) == "" {
			return nil, fmt.Errorf("AUTH_USERS entry %d requires a username and password separated by a colon", i+1)
		}
		if _, exists := users[username]; exists {
			return nil, fmt.Errorf("AUTH_USERS entry %d repeats a username", i+1)
		}
		users[username] = sha256.Sum256([]byte(password))
	}
	return users, nil
}

// Both HTTP actions and WebSocket handshakes authenticate before reaching their
// handlers. Only the WebSocket endpoint accepts a token in its query string.
func (a *App) requireSession(hostOnly bool) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			token := strings.TrimPrefix(c.Request().Header.Get("Authorization"), "Bearer ")
			if c.Path() == "/api/live" {
				token = c.QueryParam("token")
			}
			session, err := a.store.Session(c.Request().Context(), token)
			if session.Role == "host" {
				if _, exists := a.users[session.Username]; !exists {
					err = errors.New("account is no longer configured")
				}
			}
			if err != nil {
				return echo.NewHTTPError(http.StatusUnauthorized, "session expired; sign in or join again")
			}
			if hostOnly && session.Role != "host" {
				return echo.NewHTTPError(http.StatusForbidden, "host access required")
			}
			c.Set("session", session)
			return next(c)
		}
	}
}
