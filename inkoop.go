package exactonline

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ErrAlreadyUploaded is returned when Exact Online reports the file was already uploaded.
var ErrAlreadyUploaded = errors.New("file already uploaded")

// InkoopItem represents an open purchase invoice document in Exact Online.
type InkoopItem struct {
	Date        string
	Description string
	Amount      string
	Status      string
}

// ListOpenInkoop returns all non-"Geboekt" inkoop factuur items.
func (c *Client) ListOpenInkoop() ([]InkoopItem, error) {
	pageURL := c.url() + "/docs/PurPurchInvoiceDocuments.aspx?IsMyFirmNavigation=true&_Division_=" + c.divisionID
	resp, err := c.http.Get(pageURL)
	if err != nil {
		return nil, fmt.Errorf("GET inkoop page: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	return parseInkoopItems(string(body))
}

func parseInkoopItems(html string) ([]InkoopItem, error) {
	rowRe := regexp.MustCompile(`(?s)<tr[^>]*class="[^"]*Row[^"]*"[^>]*>(.*?)</tr>`)
	cellRe := regexp.MustCompile(`(?s)<td[^>]*>(.*?)</td>`)

	var items []InkoopItem
	for _, rowMatch := range rowRe.FindAllStringSubmatch(html, -1) {
		cells := cellRe.FindAllStringSubmatch(rowMatch[1], -1)
		if len(cells) < 3 {
			continue
		}
		item := InkoopItem{}
		for i, cell := range cells {
			val := strings.TrimSpace(stripTags(cell[1]))
			switch i {
			case 0:
				item.Date = val
			case 1:
				item.Description = val
			case 2:
				item.Amount = val
			case 3:
				item.Status = val
			}
		}
		if strings.Contains(strings.ToLower(item.Status), "geboekt") {
			continue
		}
		items = append(items, item)
	}
	return items, nil
}

var tagRe = regexp.MustCompile(`<[^>]+>`)

func stripTags(s string) string {
	return tagRe.ReplaceAllString(s, "")
}

// UploadResult holds the outcome of uploading a single file.
type UploadResult struct {
	File            string
	AlreadyUploaded bool
	Err             error
}

// UploadInkoop uploads purchase invoice files to Exact Online in batches.
// It does not stop on already-uploaded files; real errors are collected in the results.
func (c *Client) UploadInkoop(files []string) ([]UploadResult, error) {
	const maxBatch = 15

	uploadURL, err := c.getUploadURL()
	if err != nil {
		return nil, fmt.Errorf("get upload URL: %w", err)
	}

	var results []UploadResult
	for i := 0; i < len(files); i += maxBatch {
		batch := files[i:min(i+maxBatch, len(files))]

		for _, f := range batch {
			log.Printf("Uploading %s...", filepath.Base(f))
			err := c.uploadFile(f, uploadURL)
			switch {
			case errors.Is(err, ErrAlreadyUploaded):
				log.Printf("%s already uploaded, skipping", filepath.Base(f))
				results = append(results, UploadResult{File: f, AlreadyUploaded: true})
			case err != nil:
				return results, fmt.Errorf("upload %s: %w", filepath.Base(f), err)
			default:
				results = append(results, UploadResult{File: f})
			}
		}
	}
	return results, nil
}

// getUploadURL fetches the purchase invoice upload page and extracts the Dropzone URL.
// Uses IsPurchaseOverview=true as observed in the captured session.
func (c *Client) getUploadURL() (string, error) {
	// First visit a page that establishes the call stack context
	portalResp, err := c.http.Get(c.url() + "/docs/PurPurchInvoiceDocuments.aspx?IsMyFirmNavigation=true&_Division_=" + c.divisionID)
	if err != nil {
		return "", fmt.Errorf("GET portal: %w", err)
	}
	_, _ = io.ReadAll(portalResp.Body)
	_ = portalResp.Body.Close()

	pageURL := c.url() + "/docs/ClientPortalFinancialUpload.aspx?Type=20&IsPurchaseOverview=true&IsModal=1&BeginModalCallStack=1&_Division_=" + c.divisionID
	log.Printf("GET upload page: %s", pageURL)
	resp, err := c.http.Get(pageURL)
	if err != nil {
		return "", err
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	log.Printf("Upload page: status %d, %d bytes", resp.StatusCode, len(body))

	// Extract the Dropzone upload URL from the page JS
	dzURLRe := regexp.MustCompile(`new Dropzone\([^{]*\{url:\s*["']([^"']+)["']`)
	m := dzURLRe.FindStringSubmatch(string(body))
	if m == nil {
		dzURLRe = regexp.MustCompile(`url:\s*["'](/handlers/OcrService[^"']+)["']`)
		m = dzURLRe.FindStringSubmatch(string(body))
	}
	if m == nil {
		return "", fmt.Errorf("dropzone upload URL not found in upload page (%d bytes)", len(body))
	}

	uploadURL := m[1]

	// Append required params if not already present
	if !strings.Contains(uploadURL, "Subject=") {
		uploadURL += "&Subject=&Type=20&InvoiceLines="
	}

	// Append _CSL_ and _csx_ from the page
	cslRe := regexp.MustCompile(`_CSL_=(\d+)`)
	csxRe := regexp.MustCompile(`_csx_=(\d+)`)
	if cm := cslRe.FindStringSubmatch(string(body)); cm != nil && !strings.Contains(uploadURL, "_CSL_") {
		uploadURL += "&_CSL_=" + cm[1]
	}
	if cm := csxRe.FindStringSubmatch(string(body)); cm != nil && !strings.Contains(uploadURL, "_csx_") {
		uploadURL += "&_csx_=" + cm[1]
	}
	if !strings.HasPrefix(uploadURL, "http") {
		uploadURL = c.url() + uploadURL
	}
	log.Printf("Upload URL: %s", uploadURL)
	return uploadURL, nil
}

func (c *Client) uploadFile(filePath, uploadURL string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("file", filepath.Base(filePath))
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, f); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}

	req, err := http.NewRequest("POST", uploadURL, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", c.url())
	req.Header.Set("Referer", c.url()+"/docs/ClientPortalFinancialUpload.aspx?Type=20&IsPurchaseOverview=true&_Division_="+c.divisionID)

	// Don't follow redirects — OcrService may 302 on success
	c.http.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := c.http.Do(req)
	c.http.CheckRedirect = nil
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	// The OcrService may return:
	// - 200 with JSON {"MailMessageId": "..."} on success
	// - 302 redirect (which we stopped following) — follow it to get the actual response
	if resp.StatusCode == http.StatusFound {
		loc := resp.Header.Get("Location")
		if loc != "" {
			if !strings.HasPrefix(loc, "http") {
				loc = c.url() + loc
			}
			resp, err = c.http.Get(loc)
			if err != nil {
				return fmt.Errorf("follow upload redirect: %w", err)
			}
			defer func() { _ = resp.Body.Close() }()
			body, _ = io.ReadAll(resp.Body)
		}
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		MailMessageID string `json:"MailMessageId"`
		Errors        []struct {
			Message string `json:"Message"`
		} `json:"Errors"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("unexpected response: %s", string(body))
	}
	if result.MailMessageID == "" {
		for _, e := range result.Errors {
			if strings.Contains(e.Message, "al geüpload") {
				return ErrAlreadyUploaded
			}
		}
		return fmt.Errorf("no MailMessageId in response: %s", string(body))
	}

	log.Printf("Uploaded %s (ID: %s)", filepath.Base(filePath), result.MailMessageID)
	return nil
}
