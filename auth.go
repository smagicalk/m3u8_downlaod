package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	adminUsername   = "admin"
	sessionCookie   = "m3u8_session"
	sessionLifetime = 24 * time.Hour
)

type authService struct {
	store    *store
	mu       sync.Mutex
	sessions map[[32]byte]time.Time
}

func newAuthService(storage *store) *authService {
	return &authService{store: storage, sessions: make(map[[32]byte]time.Time)}
}

func (s *store) ensureAdmin(initialPassword string) (string, error) {
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE username = ?`, adminUsername).Scan(&count); err != nil {
		return "", err
	}
	if count > 0 {
		return "", nil
	}
	password := strings.TrimSpace(initialPassword)
	generated := ""
	if password == "" {
		var err error
		password, err = newPassword()
		if err != nil {
			return "", err
		}
		generated = password
	}
	if err := validatePassword(password); err != nil {
		return "", err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	if _, err := s.db.Exec(`INSERT INTO users(username, password_hash, updated_at) VALUES (?, ?, ?)`, adminUsername, hash, time.Now().UnixMilli()); err != nil {
		return "", err
	}
	return generated, nil
}

func (s *store) verifyPassword(username, password string) bool {
	var hash []byte
	if err := s.db.QueryRow(`SELECT password_hash FROM users WHERE username = ?`, username).Scan(&hash); err != nil {
		return false
	}
	return bcrypt.CompareHashAndPassword(hash, []byte(password)) == nil
}

func (s *store) changePassword(username, currentPassword, nextPassword string) error {
	if !s.verifyPassword(username, currentPassword) {
		return errors.New("当前密码不正确")
	}
	if err := validatePassword(nextPassword); err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(nextPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE users SET password_hash = ?, updated_at = ? WHERE username = ?`, hash, time.Now().UnixMilli(), username)
	return err
}

func validatePassword(password string) error {
	if len(password) < 8 || len(password) > 128 {
		return errors.New("密码长度必须为 8 至 128 个字符")
	}
	return nil
}

func newPassword() (string, error) {
	bytes := make([]byte, 18)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

func (a *authService) login(writer http.ResponseWriter, request *http.Request) error {
	var payload loginRequest
	if err := decodeJSONBody(writer, request, 4*1024, &payload); err != nil {
		return errors.New("请求内容无效")
	}
	if payload.Username != adminUsername || !a.store.verifyPassword(payload.Username, payload.Password) {
		return errors.New("用户名或密码错误")
	}
	value, err := newSessionToken()
	if err != nil {
		return err
	}
	hash := sha256.Sum256([]byte(value))
	a.mu.Lock()
	a.sessions[hash] = time.Now().Add(sessionLifetime)
	a.mu.Unlock()
	http.SetCookie(writer, &http.Cookie{Name: sessionCookie, Value: value, Path: "/", MaxAge: int(sessionLifetime.Seconds()), HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: request.TLS != nil})
	return nil
}

func (a *authService) logout(writer http.ResponseWriter, request *http.Request) {
	if cookie, err := request.Cookie(sessionCookie); err == nil {
		hash := sha256.Sum256([]byte(cookie.Value))
		a.mu.Lock()
		delete(a.sessions, hash)
		a.mu.Unlock()
	}
	http.SetCookie(writer, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: request.TLS != nil})
}

func (a *authService) invalidateAll() {
	a.mu.Lock()
	a.sessions = make(map[[32]byte]time.Time)
	a.mu.Unlock()
}

func (a *authService) authorized(request *http.Request) bool {
	cookie, err := request.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		return false
	}
	hash := sha256.Sum256([]byte(cookie.Value))
	a.mu.Lock()
	defer a.mu.Unlock()
	expiresAt, exists := a.sessions[hash]
	if !exists || time.Now().After(expiresAt) {
		delete(a.sessions, hash)
		return false
	}
	return true
}

func (a *authService) require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if a.authorized(request) {
			next.ServeHTTP(writer, request)
			return
		}
		if strings.HasPrefix(request.URL.Path, "/api/") {
			writeError(writer, http.StatusUnauthorized, "请先登录")
			return
		}
		http.Redirect(writer, request, "/login", http.StatusSeeOther)
	})
}

func newSessionToken() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}
