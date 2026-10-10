//go:build linux

package siwcauth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"time"
)

// Login performs one explicit interactive Authorization Code + PKCE flow.
// Tests inject local endpoints and a callback that supplies the synthetic
// browser result; the production caller opens only the system browser.
func Login(ctx context.Context, store *Store, client *http.Client, issuedClientID string, openBrowser func(context.Context, string) error) (Registration, error) {
	return loginWithEndpoints(ctx, store, client, OpenAIEndpoints(), issuedClientID, openBrowser)
}

func loginWithEndpoints(ctx context.Context, store *Store, client *http.Client, endpoints Endpoints, issuedClientID string, openBrowser func(context.Context, string) error) (resultReg Registration, resultErr error) {
	if store == nil || openBrowser == nil {
		return Registration{}, errors.New("SIWC login configuration is incomplete")
	}
	hostID, err := store.HostID(true)
	if err != nil {
		return Registration{}, err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return Registration{}, errors.New("cannot bind SIWC loopback callback")
	}
	defer func() {
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			resultErr = errors.Join(resultErr, errors.New("cannot close SIWC callback listener"))
		}
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	redirect := "http://127.0.0.1:" + fmt.Sprint(port) + "/auth/callback"
	pending, err := NewAuthorizationWithEndpoints(redirect, hostID, issuedClientID, endpoints)
	if err != nil {
		return Registration{}, err
	}
	type result struct {
		callback Callback
		err      error
	}
	resultCh := make(chan result, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/auth/callback" {
			http.Error(w, "Invalid callback", http.StatusBadRequest)
			return
		}
		callback, err := ParseCallback("http://127.0.0.1:"+strconv.Itoa(port)+r.URL.RequestURI(), pending.RedirectURI, pending.State, pending.ClientID, pending.FirstRegistration)
		if err != nil {
			http.Error(w, "Sign-in was refused. Return to Runstead.", http.StatusBadRequest)
			select {
			case resultCh <- result{callback, err}:
			default:
			}
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("Sign-in received. Return to Runstead.")); err != nil {
			select {
			case resultCh <- result{err: errors.New("cannot acknowledge SIWC callback")}:
			default:
			}
			return
		}
		select {
		case resultCh <- result{callback: callback}:
		default:
		}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 3 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			resultErr = errors.Join(resultErr, errors.New("cannot stop SIWC callback server"))
		}
	}()
	if err := openBrowser(ctx, pending.URL); err != nil {
		return Registration{}, errors.New("cannot open system browser for SIWC sign-in")
	}
	select {
	case <-ctx.Done():
		return Registration{}, ctx.Err()
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return Registration{}, errors.New("SIWC callback server stopped unexpectedly")
		}
		return Registration{}, errors.New("SIWC callback server stopped before authentication")
	case result := <-resultCh:
		if result.err != nil {
			return Registration{}, result.err
		}
		reg, err := pending.Exchange(ctx, client, endpoints, result.callback, time.Now().UTC())
		if err != nil {
			return Registration{}, err
		}
		reg.HostID = hostID
		if err := store.Save(reg); err != nil {
			return Registration{}, err
		}
		return reg, nil
	}
}

// OpenSystemBrowser uses the fixed xdg-open executable and a single URL
// argument. The URL must not be logged because it contains OAuth state.
func OpenSystemBrowser(ctx context.Context, rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New("invalid SIWC authorization URL")
	}
	path, err := exec.LookPath("xdg-open")
	if err != nil {
		return errors.New("xdg-open is unavailable")
	}
	cmd := exec.CommandContext(ctx, path, rawURL)
	if err := cmd.Run(); err != nil {
		return errors.New("cannot start system browser")
	}
	return nil
}
