package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	exactonline "github.com/gwillem/exactonline-go"
	flags "github.com/jessevdk/go-flags"
	"golang.org/x/term"
)

var opts struct {
	Verbose bool `short:"v" long:"verbose" description:"Verbose output"`
}

func main() {
	parser := flags.NewParser(&opts, flags.Default)
	parser.AddCommand("login", "Authenticate and store credentials", "", &LoginCmd{})
	parser.AddCommand("inkoop", "Purchase invoice operations", "", &InkoopCmd{})

	if _, err := parser.Parse(); err != nil {
		if flagsErr, ok := err.(*flags.Error); ok && flagsErr.Type == flags.ErrHelp {
			os.Exit(0)
		}
		os.Exit(1)
	}
}

func newClient() (*exactonline.Client, error) {
	username, password, totpSecret, err := loadCredentials()
	if err != nil {
		return nil, fmt.Errorf("no login credentials found, run: exact login")
	}

	// Try cached session first
	if cookies := loadCookies(); len(cookies) > 0 {
		c := exactonline.NewClientWithCookies(cookies)
		if c.SessionValid() {
			log.Println("Reusing cached session")
			return c, nil
		}
		log.Println("Cached session expired, re-authenticating...")
	}

	c, err := exactonline.NewClient(username, password, totpSecret)
	if err != nil {
		return nil, err
	}
	saveCookies(c.Cookies())
	return c, nil
}

// cachePath returns a path under the XDG cache directory.
func cachePath(name string) string {
	dir := os.Getenv("XDG_CACHE_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".cache")
	}
	return filepath.Join(dir, "exact-online", name)
}

type savedCredentials struct {
	Username   string `json:"username"`
	Password   string `json:"password"`
	TOTPSecret string `json:"totp_secret"`
}

func saveCredentials(username, password, totpSecret string) error {
	creds := savedCredentials{
		Username:   username,
		Password:   password,
		TOTPSecret: totpSecret,
	}
	data, err := json.Marshal(creds)
	if err != nil {
		return err
	}
	path := cachePath("credentials.json")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	return os.WriteFile(path, data, 0o600)
}

func loadCredentials() (username, password, totpSecret string, err error) {
	data, err := os.ReadFile(cachePath("credentials.json"))
	if err != nil {
		return "", "", "", err
	}
	var creds savedCredentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return "", "", "", err
	}
	if creds.Username == "" || creds.Password == "" || creds.TOTPSecret == "" {
		return "", "", "", fmt.Errorf("incomplete credentials")
	}
	return creds.Username, creds.Password, creds.TOTPSecret, nil
}

type savedCookie struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain"`
	Path   string `json:"path"`
}

func saveCookies(cookies []*http.Cookie) {
	var saved []savedCookie
	for _, ck := range cookies {
		saved = append(saved, savedCookie{
			Name:   ck.Name,
			Value:  ck.Value,
			Domain: ck.Domain,
			Path:   ck.Path,
		})
	}
	data, err := json.Marshal(saved)
	if err != nil {
		return
	}
	path := cachePath("cookies.json")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, data, 0o600)
}

func loadCookies() []*http.Cookie {
	data, err := os.ReadFile(cachePath("cookies.json"))
	if err != nil {
		return nil
	}
	var saved []savedCookie
	if err := json.Unmarshal(data, &saved); err != nil {
		return nil
	}
	cookies := make([]*http.Cookie, len(saved))
	for i, s := range saved {
		cookies[i] = &http.Cookie{
			Name:   s.Name,
			Value:  s.Value,
			Domain: s.Domain,
			Path:   s.Path,
		}
	}
	return cookies
}

func init() {
	log.SetOutput(io.Discard)
}

func setupLogging() {
	if opts.Verbose {
		log.SetOutput(os.Stderr)
	}
}

// LoginCmd prompts for credentials, tests login, and stores them.
type LoginCmd struct{}

func (cmd *LoginCmd) Execute(args []string) error {
	setupLogging()

	reader := bufio.NewReader(os.Stdin)

	fmt.Print("Username (email): ")
	username, _ := reader.ReadString('\n')
	username = strings.TrimSpace(username)

	fmt.Print("Password: ")
	passwordBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("reading password: %w", err)
	}
	fmt.Println()
	password := string(passwordBytes)

	fmt.Print("TOTP secret: ")
	totpSecret, _ := reader.ReadString('\n')
	totpSecret = strings.TrimSpace(totpSecret)

	fmt.Println("Testing login...")
	c, err := exactonline.NewClient(username, password, totpSecret)
	if err != nil {
		return err
	}

	if err := saveCredentials(username, password, totpSecret); err != nil {
		return fmt.Errorf("saving credentials: %w", err)
	}
	saveCookies(c.Cookies())

	fmt.Println("Login successful! Credentials saved.")
	return nil
}

// InkoopCmd is the parent command for inkoop subcommands.
type InkoopCmd struct {
	List   InkoopListCmd   `command:"list" description:"List open (non-Geboekt) inkoop facturen"`
	Upload InkoopUploadCmd `command:"upload" description:"Upload inkoop facturen"`
}

type InkoopListCmd struct{}

func (cmd *InkoopListCmd) Execute(args []string) error {
	setupLogging()
	client, err := newClient()
	if err != nil {
		return err
	}

	items, err := client.ListOpenInkoop()
	if err != nil {
		return err
	}

	if len(items) == 0 {
		fmt.Println("No open inkoop facturen found.")
		return nil
	}

	for _, item := range items {
		fmt.Printf("%s\t%s\t%s\t%s\n", item.Date, item.Amount, item.Status, item.Description)
	}
	return nil
}

type InkoopUploadCmd struct {
	Args struct {
		Files []string `positional-arg-name:"FILES" required:"true" description:"PDF/TIF/JPG files to upload"`
	} `positional-args:"yes"`
}

var validExts = map[string]bool{
	".pdf": true, ".tif": true, ".tiff": true, ".jpg": true, ".jpeg": true,
}

func (cmd *InkoopUploadCmd) Execute(args []string) error {
	setupLogging()

	// Validate files
	for _, f := range cmd.Args.Files {
		ext := strings.ToLower(filepath.Ext(f))
		if !validExts[ext] {
			return fmt.Errorf("unsupported file type %q (accepted: PDF, TIF, TIFF, JPG, JPEG)", ext)
		}
		if _, err := os.Stat(f); err != nil {
			return fmt.Errorf("file not found: %s", f)
		}
	}

	client, err := newClient()
	if err != nil {
		return err
	}

	return client.UploadInkoop(cmd.Args.Files)
}
