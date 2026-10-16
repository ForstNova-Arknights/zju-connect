package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/mythologyli/zju-connect/client/authchallenge"
	"github.com/mythologyli/zju-connect/log"
)

const casUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36 Edg/149.0.0.0"

// CASLoginOptions configures the automatic CAS login.
type CASLoginOptions struct {
	Username string
	Password string
	// Domain is the CAS login domain, sent as the sfDomain query parameter of
	// the portal callback URL. It matches the AuthInfo.LoginDomain / LoginDomain().
	Domain string
}

// NewCASAutoChallengeHandler returns a handler that resolves the CAS external
// login by performing the full CAS flow itself, instead of asking the user to
// paste the callback URL. All other challenges are delegated to base.
func NewCASAutoChallengeHandler(base authchallenge.Handler, options CASLoginOptions) authchallenge.Handler {
	if base == nil {
		base = authchallenge.NewCLIHandler(authchallenge.CLIOptions{})
	}
	auto := &casAutoLoginHandler{
		username:    options.Username,
		password:    options.Password,
		domain:      options.Domain,
		fpVisitorID: randomHex(16),
		client:      newCASHTTPClient(),
	}
	return casChallengeHandler{base: base, auto: auto}
}

type casChallengeHandler struct {
	base authchallenge.Handler
	auto *casAutoLoginHandler
}

func (h casChallengeHandler) HandleCodeChallenge(challenge authchallenge.CodeChallenge) (authchallenge.CodeResponse, error) {
	return h.base.HandleCodeChallenge(challenge)
}

func (h casChallengeHandler) HandleTextCaptcha(challenge authchallenge.TextCaptchaChallenge) (authchallenge.TextCaptchaResponse, error) {
	return h.base.HandleTextCaptcha(challenge)
}

func (h casChallengeHandler) HandleClickCaptcha(challenge authchallenge.ClickCaptchaChallenge) (authchallenge.ClickCaptchaResponse, error) {
	return h.base.HandleClickCaptcha(challenge)
}

func (h casChallengeHandler) HandleExternalLogin(challenge authchallenge.ExternalLoginChallenge) (authchallenge.ExternalLoginResponse, error) {
	if challenge.Kind != authchallenge.ExternalLoginCAS {
		return h.base.HandleExternalLogin(challenge)
	}
	callback, err := h.auto.login(challenge.LoginURL)
	if err != nil {
		return authchallenge.ExternalLoginResponse{}, err
	}
	return authchallenge.ExternalLoginResponse{CallbackURL: callback}, nil
}

type casAutoLoginHandler struct {
	username    string
	password    string
	domain      string
	fpVisitorID string
	client      *http.Client
}

// login runs the CAS flow described in the SHOU adaptation doc:
//
//	GET  {portal}/passport/v1/public/casLogin?sfDomain=... -> 302 CAS login url
//	GET  {cas}/cas/login?service=...                       -> ST via SSO, or form
//	GET  {cas}/cas/jwt/publicKey                           -> PEM (form path)
//	POST {cas}/cas/login?service=...                       -> ST (form path)
//
// It returns the portal callback URL carrying the ST ticket.
func (h *casAutoLoginHandler) login(loginURL string) (string, error) {
	portal, err := url.Parse(loginURL)
	if err != nil {
		return "", fmt.Errorf("cas: invalid casLogin url %q: %w", loginURL, err)
	}
	if portal.Scheme == "" || portal.Host == "" {
		return "", fmt.Errorf("cas: casLogin url is not absolute: %q", loginURL)
	}
	portalBase := portal.Scheme + "://" + portal.Host
	sfDomain := h.domain
	if sfDomain == "" {
		sfDomain = portal.Query().Get("sfDomain")
	}

	log.Println("CAS: request portal casLogin")
	_, status, location, err := h.get(loginURL)
	if err != nil {
		return "", fmt.Errorf("cas: casLogin request: %w", err)
	}
	if location == "" {
		return "", fmt.Errorf("cas: casLogin returned no redirect (HTTP %d)", status)
	}
	casLoginURL, err := url.Parse(location)
	if err != nil {
		return "", fmt.Errorf("cas: invalid cas login url %q: %w", location, err)
	}
	service := casLoginURL.Query().Get("service")
	if service == "" {
		return "", fmt.Errorf("cas: cas login url has no service parameter, portal may already be logged in")
	}
	casBase := casLoginURL.Scheme + "://" + casLoginURL.Host

	var ticket string

	log.Println("CAS: fetch CAS login page")
	page, status, casLocation, err := h.get(casLoginURL.String())
	if err != nil {
		return "", fmt.Errorf("cas: fetch CAS login page: %w", err)
	}
	if isRedirect(status) {
		if ticket = ticketFromLocation(casLocation); ticket != "" {
			log.Println("CAS: SSO session still valid, obtained ticket without password")
		}
	}

	if ticket == "" {
		ticket, err = h.submitCredentials(casBase, service, page)
		if err != nil {
			return "", err
		}
	}

	params := url.Values{
		"sfDomain": {sfDomain},
		"ticket":   {ticket},
	}
	log.Printf("CAS: obtained ticket %s", ticket)
	return portalBase + "/passport/v1/auth/cas?" + params.Encode(), nil
}

func (h *casAutoLoginHandler) submitCredentials(casBase, service string, page []byte) (string, error) {
	execution := extractExecution(string(page))
	if execution == "" {
		return "", fmt.Errorf("cas: no execution token found in CAS login page")
	}
	if h.username == "" || h.password == "" {
		return "", fmt.Errorf("cas: username and password are required for CAS form login")
	}

	log.Println("CAS: fetch CAS public key")
	publicKey, _, _, err := h.get(casBase + "/cas/jwt/publicKey")
	if err != nil {
		return "", fmt.Errorf("cas: fetch CAS public key: %w", err)
	}
	encryptedPassword, err := encryptCASPassword(h.password, string(publicKey))
	if err != nil {
		return "", err
	}

	form := url.Values{
		"username":    {h.username},
		"password":    {encryptedPassword},
		"captcha":     {""},
		"mfaState":    {""},
		"currentMenu": {"1"},
		"failN":       {"-1"},
		"execution":   {execution},
		"_eventId":    {"submit"},
		"geolocation": {""},
		"fpVisitorId": {h.fpVisitorID},
		"submit":      {"Login1"},
	}
	postURL := casBase + "/cas/login?" + url.Values{"service": {service}}.Encode()

	log.Println("CAS: submit credentials")
	_, status, location, err := h.postForm(postURL, form)
	if err != nil {
		return "", fmt.Errorf("cas: submit CAS login: %w", err)
	}
	ticket := ticketFromLocation(location)
	if ticket == "" {
		return "", fmt.Errorf("cas: CAS login did not return a ticket (HTTP %d, location %q)", status, location)
	}
	log.Println("CAS: obtained ticket via form login")
	return ticket, nil
}

func (h *casAutoLoginHandler) get(rawURL string) (body []byte, status int, location string, err error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, "", err
	}
	h.setPageHeaders(req)
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, resp.Header.Get("Location"), err
	}
	return body, resp.StatusCode, resp.Header.Get("Location"), nil
}

func (h *casAutoLoginHandler) postForm(rawURL string, form url.Values) (body []byte, status int, location string, err error) {
	req, err := http.NewRequest(http.MethodPost, rawURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.setPageHeaders(req)
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, resp.Header.Get("Location"), err
	}
	return body, resp.StatusCode, resp.Header.Get("Location"), nil
}

func (h *casAutoLoginHandler) setPageHeaders(req *http.Request) {
	req.Header.Set("User-Agent", casUserAgent)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
}

func newCASHTTPClient() *http.Client {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Transport: transport,
		Jar:       jar,
		Timeout:   30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

var executionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`name="execution"\s+value="([^"]+)"`),
	regexp.MustCompile(`value="([^"]+)"\s+name="execution"`),
}

func extractExecution(page string) string {
	for _, pattern := range executionPatterns {
		if match := pattern.FindStringSubmatch(page); len(match) > 1 {
			return match[1]
		}
	}
	return ""
}

func encryptCASPassword(password, publicKeyPEM string) (string, error) {
	block, _ := pem.Decode([]byte(publicKeyPEM))
	if block == nil {
		return "", fmt.Errorf("cas: invalid RSA public key PEM")
	}
	publicKey, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("cas: parse RSA public key: %w", err)
	}
	rsaPublicKey, ok := publicKey.(*rsa.PublicKey)
	if !ok {
		return "", fmt.Errorf("cas: public key is not RSA")
	}
	ciphertext, err := rsa.EncryptPKCS1v15(rand.Reader, rsaPublicKey, []byte(password))
	if err != nil {
		return "", fmt.Errorf("cas: encrypt password: %w", err)
	}
	return "__RSA__" + base64.StdEncoding.EncodeToString(ciphertext), nil
}

func ticketFromLocation(location string) string {
	if location == "" {
		return ""
	}
	u, err := url.Parse(location)
	if err != nil {
		return ""
	}
	return u.Query().Get("ticket")
}

func isRedirect(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	default:
		return false
	}
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return hex.EncodeToString(buf)
}
