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
	"time"

	exactonline "github.com/gwillem/exactonline-go"
	flags "github.com/jessevdk/go-flags"
	"golang.org/x/term"
)

var opts struct {
	Verbose bool `short:"v" long:"verbose" description:"Verbose output"`
}

func main() {
	parser := flags.NewParser(&opts, flags.Default)
	if _, err := parser.AddCommand("login", "Authenticate and store credentials", "", &LoginCmd{}); err != nil {
		log.Fatal(err)
	}
	if _, err := parser.AddCommand("inkoop", "Purchase invoice operations", "", &InkoopCmd{}); err != nil {
		log.Fatal(err)
	}
	if _, err := parser.AddCommand("report", "Financial report operations", "", &ReportCmd{}); err != nil {
		log.Fatal(err)
	}

	parser.CommandHandler = func(cmd flags.Commander, args []string) error {
		if opts.Verbose {
			log.SetOutput(os.Stderr)
		}
		return cmd.Execute(args)
	}

	if _, err := parser.Parse(); err != nil {
		if flagsErr, ok := err.(*flags.Error); ok && flagsErr.Type == flags.ErrHelp {
			os.Exit(0)
		}
		os.Exit(1)
	}
}

func newClient() (*exactonline.Client, error) {
	creds, err := loadCredentials()
	if err != nil {
		return nil, fmt.Errorf("no login credentials found, run: exact login")
	}

	// Try cached session first
	if creds.DivisionID != "" {
		if cookies := loadCookies(); len(cookies) > 0 {
			c := exactonline.NewClientWithCookies(cookies, creds.DivisionID)
			if c.SessionValid() {
				log.Println("Reusing cached session")
				return c, nil
			}
			log.Println("Cached session expired, re-authenticating...")
		}
	}

	c, err := exactonline.NewClient(creds.Username, creds.Password, creds.TOTPSecret)
	if err != nil {
		return nil, err
	}
	saveCookies(c.Cookies())
	creds.DivisionID = c.DivisionID()
	_ = saveCredentials(creds)
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
	DivisionID string `json:"division_id,omitempty"`
}

func saveCredentials(creds savedCredentials) error {
	data, err := json.Marshal(creds)
	if err != nil {
		return err
	}
	path := cachePath("credentials.json")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	return os.WriteFile(path, data, 0o600)
}

func loadCredentials() (savedCredentials, error) {
	data, err := os.ReadFile(cachePath("credentials.json"))
	if err != nil {
		return savedCredentials{}, err
	}
	var creds savedCredentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return savedCredentials{}, err
	}
	if creds.Username == "" || creds.Password == "" || creds.TOTPSecret == "" {
		return savedCredentials{}, fmt.Errorf("incomplete credentials")
	}
	return creds, nil
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

// LoginCmd prompts for credentials, tests login, and stores them.
type LoginCmd struct{}

func (cmd *LoginCmd) Execute(args []string) error {
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

	creds := savedCredentials{
		Username:   username,
		Password:   password,
		TOTPSecret: totpSecret,
		DivisionID: c.DivisionID(),
	}
	if err := saveCredentials(creds); err != nil {
		return fmt.Errorf("saving credentials: %w", err)
	}
	saveCookies(c.Cookies())

	fmt.Printf("Login successful! Division %s, credentials saved.\n", c.DivisionID())
	return nil
}

// InkoopCmd is the parent command for inkoop subcommands.
type InkoopCmd struct {
	List   InkoopListCmd   `command:"list" description:"List open (non-Geboekt) inkoop facturen"`
	Upload InkoopUploadCmd `command:"upload" description:"Upload inkoop facturen"`
}

type InkoopListCmd struct{}

func (cmd *InkoopListCmd) Execute(args []string) error {
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

	results, err := client.UploadInkoop(cmd.Args.Files)
	moveUploadedFiles(results)
	return err
}

func moveUploadedFiles(results []exactonline.UploadResult) int {
	moved := 0
	for _, r := range results {
		if r.Err != nil {
			continue
		}
		dir := filepath.Join(filepath.Dir(r.File), "uploaded")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not create %s: %v\n", dir, err)
			continue
		}
		dest := filepath.Join(dir, filepath.Base(r.File))
		if err := os.Rename(r.File, dest); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not move %s to %s: %v\n", r.File, dest, err)
			continue
		}
		if r.AlreadyUploaded {
			fmt.Printf("Already uploaded, moved to %s\n", dest)
		} else {
			fmt.Printf("Uploaded, moved to %s\n", dest)
		}
		moved++
	}
	return moved
}

// ReportCmd is the parent command for report subcommands.
type ReportCmd struct {
	Download ReportDownloadCmd `command:"download" description:"Download financial report as Excel"`
}

// ReportDownloadCmd downloads a financial report.
type ReportDownloadCmd struct {
	Type   string `long:"type" default:"profitloss" choice:"profitloss" choice:"balancesheet" description:"Report type"`
	Year   int    `long:"year" description:"Fiscal year (default: current)"`
	From   int    `long:"from" default:"1" description:"Start period (1-12)"`
	To     int    `long:"to" default:"12" description:"End period (1-12)"`
	Output string `short:"o" long:"output" description:"Output filename (default: auto from server)"`
}

func (cmd *ReportDownloadCmd) Execute(args []string) error {
	if cmd.Year == 0 {
		cmd.Year = time.Now().Year()
	}
	if cmd.From < 1 || cmd.From > 12 || cmd.To < 1 || cmd.To > 12 || cmd.From > cmd.To {
		return fmt.Errorf("invalid period range: %d-%d (must be 1-12, from <= to)", cmd.From, cmd.To)
	}

	client, err := newClient()
	if err != nil {
		return err
	}

	rt := exactonline.ReportType(cmd.Type)
	data, filename, err := client.DownloadReport(rt, cmd.Year, cmd.From, cmd.To)
	if err != nil {
		return err
	}

	outPath := cmd.Output
	if outPath == "" {
		outPath = filename
	}
	if outPath == "" {
		outPath = fmt.Sprintf("report-%s-%d-p%d-%d.xlsx", cmd.Type, cmd.Year, cmd.From, cmd.To)
	}

	if err := os.WriteFile(outPath, data, 0o644); err != nil {
		return fmt.Errorf("write file: %w", err)
	}
	fmt.Printf("Downloaded %s (%d bytes)\n", outPath, len(data))
	return nil
}
