package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mythologyli/zju-connect/client/authchallenge"
)

func testPublicKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func TestCASAutoLoginSSOShortcut(t *testing.T) {
	portalCallback := ""
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/passport/v1/public/casLogin":
			if r.URL.Query().Get("sfDomain") != "cas-example" {
				t.Errorf("sfDomain = %q", r.URL.Query().Get("sfDomain"))
			}
			http.Redirect(w, r, serverURL(r)+"/cas/login?service="+url.QueryEscape(portalCallback), http.StatusFound)
		case r.URL.Path == "/cas/login" && r.Method == http.MethodGet:
			http.Redirect(w, r, portalCallback+"?ticket=ST-sso-ticket", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	portalCallback = server.URL + "/passport/v1/auth/cas"

	handler := NewCASAutoChallengeHandler(nil, CASLoginOptions{Domain: "cas-example"})
	response, err := handler.HandleExternalLogin(authchallenge.ExternalLoginChallenge{
		Kind:     authchallenge.ExternalLoginCAS,
		LoginURL: server.URL + "/passport/v1/public/casLogin?sfDomain=cas-example",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := portalCallback + "?sfDomain=cas-example&ticket=ST-sso-ticket"
	if response.CallbackURL != want {
		t.Fatalf("callback = %q, want %q", response.CallbackURL, want)
	}
}

func TestCASAutoLoginFormPath(t *testing.T) {
	portalCallback := ""
	pemKey := testPublicKeyPEM(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/passport/v1/public/casLogin":
			http.Redirect(w, r, serverURL(r)+"/cas/login?service="+url.QueryEscape(portalCallback), http.StatusFound)
		case r.URL.Path == "/cas/login" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<html><body><form>` +
				`<input type="hidden" name="execution" value="e1s1"/>` +
				`</form></body></html>`))
		case r.URL.Path == "/cas/jwt/publicKey":
			_, _ = w.Write([]byte(pemKey))
		case r.URL.Path == "/cas/login" && r.Method == http.MethodPost:
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.Form.Get("username") != "testuser" {
				t.Errorf("username = %q", r.Form.Get("username"))
			}
			if r.Form.Get("execution") != "e1s1" {
				t.Errorf("execution = %q", r.Form.Get("execution"))
			}
			if !strings.HasPrefix(r.Form.Get("password"), "__RSA__") {
				t.Errorf("password = %q", r.Form.Get("password"))
			}
			if r.URL.Query().Get("service") != portalCallback {
				t.Errorf("service = %q", r.URL.Query().Get("service"))
			}
			http.Redirect(w, r, portalCallback+"?ticket=ST-form-ticket", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	portalCallback = server.URL + "/passport/v1/auth/cas"

	handler := NewCASAutoChallengeHandler(nil, CASLoginOptions{
		Username: "testuser",
		Password: "secret",
		Domain:   "cas-example",
	})
	response, err := handler.HandleExternalLogin(authchallenge.ExternalLoginChallenge{
		Kind:     authchallenge.ExternalLoginCAS,
		LoginURL: server.URL + "/passport/v1/public/casLogin?sfDomain=cas-example",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := portalCallback + "?sfDomain=cas-example&ticket=ST-form-ticket"
	if response.CallbackURL != want {
		t.Fatalf("callback = %q, want %q", response.CallbackURL, want)
	}
}

func TestCASAutoLoginDelegatesOtherKinds(t *testing.T) {
	called := false
	base := authchallenge.HandlerFuncs{
		ExternalLogin: func(challenge authchallenge.ExternalLoginChallenge) (authchallenge.ExternalLoginResponse, error) {
			called = true
			return authchallenge.ExternalLoginResponse{CallbackURL: "delegated"}, nil
		},
	}
	handler := NewCASAutoChallengeHandler(base, CASLoginOptions{})
	response, err := handler.HandleExternalLogin(authchallenge.ExternalLoginChallenge{
		Kind:     authchallenge.ExternalLoginOAuth2,
		LoginURL: "https://idp.example/oauth",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called || response.CallbackURL != "delegated" {
		t.Fatalf("delegate called = %v, callback = %q", called, response.CallbackURL)
	}
}

func TestInteractiveCASResolvesRelativeLoginURL(t *testing.T) {
	session := NewSession("vpn.example.com", nil)
	var got string
	session.challengeHandler = authchallenge.HandlerFuncs{
		ExternalLogin: func(challenge authchallenge.ExternalLoginChallenge) (authchallenge.ExternalLoginResponse, error) {
			got = challenge.LoginURL
			return authchallenge.ExternalLoginResponse{
				CallbackURL: "https://vpn.example.com/passport/v1/auth/cas?ticket=ST-x",
			}, nil
		},
	}
	if _, err := session.interactiveCas("/passport/v1/public/casLogin?sfDomain=cas-example"); err != nil {
		t.Fatal(err)
	}
	want := "https://vpn.example.com/passport/v1/public/casLogin?sfDomain=cas-example"
	if got != want {
		t.Fatalf("login url = %q, want %q", got, want)
	}
}

func serverURL(r *http.Request) string {
	scheme := "https"
	if r.TLS == nil {
		scheme = "http"
	}
	return scheme + "://" + r.Host
}
