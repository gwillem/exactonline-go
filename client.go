package exactonline

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/pquerna/otp/totp"
)

const (
	baseURL   = "https://start.exactonline.nl"
	userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36"
)

// uaTransport wraps an http.RoundTripper to set User-Agent on all requests.
type uaTransport struct {
	base http.RoundTripper
}

func (t *uaTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("User-Agent", userAgent)
	return t.base.RoundTrip(req)
}

// Client is an authenticated Exact Online HTTP client.
type Client struct {
	http       *http.Client
	divisionID string
	baseURL    string // defaults to baseURL const; overridden in tests
}

// b2cSettings holds the parsed SETTINGS object from Azure B2C login pages.
type b2cSettings struct {
	API     string `json:"api"`
	CSRF    string `json:"csrf"`
	TransID string `json:"transId"`
	Hosts   struct {
		Tenant string `json:"tenant"`
		Policy string `json:"policy"`
	} `json:"hosts"`
}

// url returns the client's base URL.
func (c *Client) url() string {
	if c.baseURL != "" {
		return c.baseURL
	}
	return baseURL
}

// NewClient creates an authenticated Exact Online client.
func NewClient(username, password, totpSecret string) (*Client, error) {
	jar, _ := cookiejar.New(nil)
	c := &Client{
		http: &http.Client{
			Transport: &uaTransport{base: http.DefaultTransport},
			Jar:       jar,
			Timeout:   30 * time.Second,
		},
	}
	if err := c.login(username, password, totpSecret); err != nil {
		return nil, fmt.Errorf("login failed: %w", err)
	}
	if err := c.detectDivision(); err != nil {
		return nil, fmt.Errorf("detect division: %w", err)
	}
	return c, nil
}

// DivisionID returns the active division ID.
func (c *Client) DivisionID() string {
	return c.divisionID
}

// NewClientWithCookies creates a client using pre-existing cookies and division ID, skipping login.
func NewClientWithCookies(cookies []*http.Cookie, divisionID string) *Client {
	jar, _ := cookiejar.New(nil)
	c := &Client{
		http: &http.Client{
			Transport: &uaTransport{base: http.DefaultTransport},
			Jar:       jar,
			Timeout:   30 * time.Second,
		},
		divisionID: divisionID,
	}
	u, _ := url.Parse(c.url())
	jar.SetCookies(u, cookies)
	return c
}

// SessionValid checks if the current session is still authenticated.
func (c *Client) SessionValid() bool {
	c.http.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := c.http.Get(c.url() + "/Dashboard/MyFirmDashboard?_Division_=" + c.divisionID)
	c.http.CheckRedirect = nil
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Cookies returns the current session cookies for persistence.
func (c *Client) Cookies() []*http.Cookie {
	u, _ := url.Parse(c.url())
	return c.http.Jar.Cookies(u)
}

// detectDivision discovers the active division ID by following the homepage redirect.
// For multi-company accounts, the redirect lands on MyFirmPortal with an empty _Division_;
// in that case we parse the page for division links and pick the first one.
func (c *Client) detectDivision() error {
	resp, err := c.http.Get(c.url() + "/")
	if err != nil {
		return err
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	q := resp.Request.URL.Query()
	div := q.Get("_Division_")
	if div != "" {
		log.Printf("Detected division ID: %s", div)
		c.divisionID = div
		return nil
	}

	// Multi-company account: parse portal page for division links
	re := regexp.MustCompile(`[?&]_Division_=(\d+)`)
	matches := re.FindAllStringSubmatch(string(body), -1)
	if len(matches) == 0 {
		return fmt.Errorf("no divisions found on portal page: %s", resp.Request.URL)
	}

	// Deduplicate
	seen := map[string]bool{}
	var divisions []string
	for _, m := range matches {
		if !seen[m[1]] {
			seen[m[1]] = true
			divisions = append(divisions, m[1])
		}
	}

	div = divisions[0]
	if len(divisions) > 1 {
		log.Printf("Found %d divisions: %v, using first: %s", len(divisions), divisions, div)
	}
	log.Printf("Detected division ID: %s", div)
	c.divisionID = div
	return nil
}

func (c *Client) login(username, password, totpSecret string) error {
	// Step 1: GET the homepage → extract anti-forgery token
	log.Println("Starting Exact Online login...")
	resp, err := c.http.Get(c.url() + "/")
	if err != nil {
		return fmt.Errorf("GET homepage: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	token := extractAttr(string(body), `name="__RequestVerificationToken"[^>]*value="([^"]+)"`)
	if token == "" {
		return fmt.Errorf("anti-forgery token not found on homepage")
	}
	log.Println("Got anti-forgery token")

	// Step 2: POST email to homepage form → redirects through Login.aspx → B2C authorize
	resp, err = c.http.PostForm(c.url()+"/", url.Values{
		"LoginForm$UserName":         {username},
		"__RequestVerificationToken": {token},
	})
	if err != nil {
		return fmt.Errorf("POST login form: %w", err)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	log.Printf("B2C page loaded from %s", resp.Request.URL.Host)

	settings, err := parseB2CSettings(string(body))
	if err != nil {
		return fmt.Errorf("parse B2C page (host=%s, status=%d): %w", resp.Request.URL.Host, resp.StatusCode, err)
	}

	b2cBase := "https://login.exact.com" + settings.Hosts.Tenant

	// Step 2: POST the fields requested by the first B2C page.
	log.Println("Submitting credentials...")
	fields := parseSAFields(string(body))
	var formData url.Values
	if len(fields) == 0 {
		// Older pages did not expose SA_FIELDS; keep the previous combined-form behavior.
		formData = url.Values{
			"signInName":   {username},
			"password":     {password},
			"request_type": {"RESPONSE"},
		}
	} else {
		formData, err = fillB2CFields(fields, username, password, totpSecret)
		if err != nil {
			return fmt.Errorf("fill B2C credentials form: %w", err)
		}
	}
	err = c.b2cPost(b2cBase, settings, formData)
	if err != nil {
		return fmt.Errorf("submit credentials: %w", err)
	}

	// Step 3: GET confirmed → next page (FetchUserAgent or TOTP)
	settings, body2, err := c.b2cConfirmed(b2cBase, settings)
	if err != nil {
		return fmt.Errorf("post-credentials confirmed: %w", err)
	}

	// Handle intermediate steps until we get the auto-submit form
	for step := range 5 {
		// Check if this is the final auto-submit form
		if strings.Contains(string(body2), "signin-azureb2c") {
			return c.submitFinalForm(string(body2))
		}

		// Determine what this step needs.
		fields := parseSAFields(string(body2))
		if len(fields) == 0 {
			return fmt.Errorf("B2C step %d has no recognized fields", step)
		}
		formData, err := fillB2CFields(fields, username, password, totpSecret)
		if err != nil {
			return fmt.Errorf("fill B2C step %d form: %w", step, err)
		}

		if containsField(fields, "userAgent") {
			log.Println("Submitting user agent...")
		} else if containsField(fields, "totpVerificationCode") {
			log.Println("Generating TOTP code...")
		} else if containsField(fields, "signInName") || containsField(fields, "email") || containsField(fields, "emailAddress") {
			log.Println("Submitting email address...")
		} else if containsField(fields, "password") {
			log.Println("Submitting password...")
		}

		settings, err = parseB2CSettings(string(body2))
		if err != nil {
			return fmt.Errorf("parse B2C step %d: %w", step, err)
		}
		b2cBase = "https://login.exact.com" + settings.Hosts.Tenant

		err = c.b2cPost(b2cBase, settings, formData)
		if err != nil {
			return fmt.Errorf("B2C step %d POST: %w", step, err)
		}

		settings, body2, err = c.b2cConfirmed(b2cBase, settings)
		if err != nil {
			return fmt.Errorf("B2C step %d confirmed: %w", step, err)
		}
	}

	return fmt.Errorf("login flow did not complete after max steps")
}

// b2cPost submits form data to the B2C SelfAsserted endpoint.
func (c *Client) b2cPost(b2cBase string, settings b2cSettings, data url.Values) error {
	endpoint := b2cBase + "/SelfAsserted"
	params := url.Values{
		"tx": {settings.TransID},
		"p":  {settings.Hosts.Policy},
	}

	req, err := http.NewRequest("POST", endpoint+"?"+params.Encode(), strings.NewReader(data.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("X-CSRF-TOKEN", settings.CSRF)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	var result struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("unexpected response: %s", string(body))
	}
	if result.Status != "200" {
		return fmt.Errorf("B2C returned status %s: %s", result.Status, string(body))
	}
	return nil
}

// b2cConfirmed calls the confirmed endpoint and returns the new settings and page body.
func (c *Client) b2cConfirmed(b2cBase string, settings b2cSettings) (b2cSettings, []byte, error) {
	endpoint := b2cBase + "/api/" + settings.API + "/confirmed"
	params := url.Values{
		"csrf_token": {settings.CSRF},
		"tx":         {settings.TransID},
		"p":          {settings.Hosts.Policy},
	}
	if settings.API == "CombinedSigninAndSignup" {
		params.Set("rememberMe", "false")
	}

	resp, err := c.http.Get(endpoint + "?" + params.Encode())
	if err != nil {
		return b2cSettings{}, nil, err
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	newSettings, err := parseB2CSettings(string(body))
	if err != nil {
		// Might be the final auto-submit form (no SETTINGS)
		return b2cSettings{}, body, nil
	}
	return newSettings, body, nil
}

// submitFinalForm parses and submits the auto-POST form that completes the B2C flow.
func (c *Client) submitFinalForm(html string) error {
	log.Println("Completing login...")

	action := extractAttr(html, `form[^>]*action=['"]([^'"]+)['"]`)
	if action == "" {
		return fmt.Errorf("no form action in final redirect page")
	}

	data := url.Values{}
	// Match hidden inputs with either single or double quotes
	re := regexp.MustCompile(`<input type=['"]hidden['"] name=['"]([^'"]+)['"][^>]*value=['"]([^'"]*)['"]`)
	for _, m := range re.FindAllStringSubmatch(html, -1) {
		data.Set(m[1], m[2])
	}

	// Don't follow redirects for this POST — we just need the cookies
	c.http.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := c.http.PostForm(action, data)
	c.http.CheckRedirect = nil
	if err != nil {
		return fmt.Errorf("POST signin callback: %w", err)
	}
	_ = resp.Body.Close()

	// Follow the redirect chain manually to collect all cookies
	for resp.StatusCode == 302 || resp.StatusCode == 301 {
		loc := resp.Header.Get("Location")
		if loc == "" {
			break
		}
		if !strings.HasPrefix(loc, "http") {
			loc = c.url() + loc
		}
		c.http.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
		resp, err = c.http.Get(loc)
		c.http.CheckRedirect = nil
		if err != nil {
			return fmt.Errorf("follow redirect: %w", err)
		}
		_ = resp.Body.Close()
	}

	log.Println("Login successful")
	return nil
}

var settingsRe = regexp.MustCompile(`var SETTINGS = ({[^;]+})`)

func parseB2CSettings(html string) (b2cSettings, error) {
	m := settingsRe.FindStringSubmatch(html)
	if m == nil {
		return b2cSettings{}, fmt.Errorf("SETTINGS not found in page")
	}
	var s b2cSettings
	if err := json.Unmarshal([]byte(m[1]), &s); err != nil {
		return b2cSettings{}, fmt.Errorf("parse SETTINGS JSON: %w", err)
	}
	return s, nil
}

var saFieldsRe = regexp.MustCompile(`var SA_FIELDS = ({[^;]+})`)

func parseSAFields(html string) []string {
	m := saFieldsRe.FindStringSubmatch(html)
	if m == nil {
		return nil
	}
	var fields struct {
		AttributeFields []struct {
			ID string `json:"ID"`
		} `json:"AttributeFields"`
	}
	if err := json.Unmarshal([]byte(m[1]), &fields); err != nil {
		return nil
	}
	var ids []string
	for _, f := range fields.AttributeFields {
		ids = append(ids, f.ID)
	}
	return ids
}

func fillB2CFields(fields []string, username, password, totpSecret string) (url.Values, error) {
	data := url.Values{"request_type": {"RESPONSE"}}
	var unknown []string

	for _, field := range fields {
		switch field {
		case "signInName", "email", "emailAddress":
			data.Set(field, username)
		case "password":
			data.Set(field, password)
		case "userAgent":
			data.Set(field, userAgent)
		case "totpVerificationCode":
			code, err := totp.GenerateCode(totpSecret, time.Now())
			if err != nil {
				return nil, fmt.Errorf("generate TOTP: %w", err)
			}
			data.Set(field, code)
		case "reset_totp":
			data.Set(field, "false")
		case "totp_skip_days":
			data.Set(field, "true")
		case "rememberMePeriodChangeInfo", "exactAssistantPayload":
			// Informational/optional fields; present in SA_FIELDS but not required for submission.
		default:
			unknown = append(unknown, field)
		}
	}

	if len(unknown) > 0 {
		return nil, fmt.Errorf("unhandled B2C fields: %v", unknown)
	}
	return data, nil
}

func containsField(fields []string, name string) bool {
	return slices.Contains(fields, name)
}

func extractAttr(html, pattern string) string {
	re := regexp.MustCompile(pattern)
	m := re.FindStringSubmatch(html)
	if m == nil {
		return ""
	}
	return m[1]
}
