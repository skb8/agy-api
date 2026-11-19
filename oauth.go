package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const googleScopes = "https://www.googleapis.com/auth/cloud-platform https://www.googleapis.com/auth/userinfo.email https://www.googleapis.com/auth/userinfo.profile https://www.googleapis.com/auth/cclog https://www.googleapis.com/auth/experimentsandconfigs"

type oauthTokens struct {
	Access  string
	Refresh string
	Expiry  time.Time
}

type googleOAuth struct {
	env      *dotenv
	client   *http.Client
	clientID string
	secret   string
	tokenURL string

	mu     sync.Mutex
	tokens oauthTokens
}

func newOAuth(env *dotenv, client *http.Client) *googleOAuth {
	expiry, _ := time.Parse(time.RFC3339, env.values["ANTIGRAVITY_TOKEN_EXPIRY"])
	return &googleOAuth{
		env:      env,
		client:   client,
		clientID: env.config("ANTIGRAVITY_OAUTH_CLIENT_ID"),
		secret:   env.config("ANTIGRAVITY_OAUTH_CLIENT_SECRET"),
		tokenURL: "https://oauth2.googleapis.com/token",
		tokens: oauthTokens{
			Access:  env.values["ANTIGRAVITY_ACCESS_TOKEN"],
			Refresh: env.values["ANTIGRAVITY_REFRESH_TOKEN"],
			Expiry:  expiry,
		},
	}
}

func (o *googleOAuth) login(ctx context.Context) error {
	if o.clientID == "" {
		return errors.New("set ANTIGRAVITY_OAUTH_CLIENT_ID in .env before signing in")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	redirect := "http://" + listener.Addr().String() + "/callback"
	state, err := randomString()
	if err != nil {
		return err
	}
	verifier, err := randomString()
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(verifier))
	query := url.Values{
		"client_id": {o.clientID}, "redirect_uri": {redirect},
		"response_type": {"code"}, "scope": {googleScopes}, "state": {state},
		"access_type": {"offline"}, "prompt": {"consent"},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	result := make(chan error, 1)
	var once sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("GET /callback", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		values := r.URL.Query()
		if subtle.ConstantTimeCompare([]byte(values.Get("state")), []byte(state)) != 1 {
			http.Error(w, "Invalid OAuth state", http.StatusBadRequest)
			return
		}
		first := false
		once.Do(func() {
			first = true
			if values.Get("error") != "" || values.Get("code") == "" {
				http.Error(w, "Google sign-in was not completed", http.StatusBadRequest)
				result <- errors.New("Google sign-in was denied or returned no code")
				return
			}
			tokens, err := o.exchange(ctx, url.Values{
				"grant_type": {"authorization_code"}, "code": {values.Get("code")},
				"redirect_uri": {redirect}, "code_verifier": {verifier},
			})
			if err == nil && tokens.Refresh == "" {
				err = errors.New("Google returned no refresh token")
			}
			if err == nil {
				err = o.persist(tokens)
			}
			if err != nil {
				http.Error(w, "Sign-in failed. Check the terminal.", http.StatusBadGateway)
			} else {
				fmt.Fprintln(w, "Signed in. You can close this tab.")
			}
			if flush, ok := w.(http.Flusher); ok {
				flush.Flush()
			}
			result <- err
		})
		if !first {
			http.Error(w, "OAuth callback already used", http.StatusConflict)
		}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if server.Shutdown(shutdown) != nil {
			server.Close()
		}
	}()
	go func() {
		if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
			select {
			case result <- err:
			default:
			}
		}
	}()
	fmt.Println("Open this URL in a browser on this computer:")
	fmt.Println("https://accounts.google.com/o/oauth2/v2/auth?" + query.Encode())
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (o *googleOAuth) accessToken(ctx context.Context, rejected string) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.tokens.Access != "" && o.tokens.Access != rejected && time.Until(o.tokens.Expiry) > time.Minute {
		return o.tokens.Access, nil
	}
	tokens, err := o.exchange(ctx, url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {o.tokens.Refresh},
	})
	if err != nil {
		return "", err
	}
	if tokens.Refresh == "" {
		tokens.Refresh = o.tokens.Refresh
	}
	if err := o.persist(tokens); err != nil {
		return "", err
	}
	return tokens.Access, nil
}

func (o *googleOAuth) exchange(ctx context.Context, form url.Values) (oauthTokens, error) {
	var result oauthTokens
	if o.clientID == "" {
		return result, errors.New("set ANTIGRAVITY_OAUTH_CLIENT_ID in .env")
	}
	form.Set("client_id", o.clientID)
	if o.secret != "" {
		form.Set("client_secret", o.secret)
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return result, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := o.client.Do(req)
	if err != nil {
		return result, fmt.Errorf("Google OAuth request: %w", err)
	}
	defer res.Body.Close()
	var payload struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
		Expires int64  `json:"expires_in"`
		Error   string `json:"error"`
	}
	decode := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&payload)
	if res.StatusCode != http.StatusOK {
		if payload.Error == "invalid_grant" && form.Get("grant_type") == "refresh_token" {
			return result, errors.New("Google authorization expired or was revoked; restart with --login")
		}
		return result, fmt.Errorf("Google OAuth returned HTTP %d", res.StatusCode)
	}
	if decode != nil || payload.Access == "" || payload.Expires <= 0 {
		return result, errors.New("invalid Google OAuth token response")
	}
	return oauthTokens{payload.Access, payload.Refresh, time.Now().Add(time.Duration(payload.Expires) * time.Second)}, nil
}

func (o *googleOAuth) persist(tokens oauthTokens) error {
	err := o.env.save(map[string]string{
		"ANTIGRAVITY_ACCESS_TOKEN":  tokens.Access,
		"ANTIGRAVITY_REFRESH_TOKEN": tokens.Refresh,
		"ANTIGRAVITY_TOKEN_EXPIRY":  tokens.Expiry.Format(time.RFC3339),
	})
	if err == nil {
		o.tokens = tokens
	}
	return err
}
