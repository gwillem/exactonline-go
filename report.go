package exactonline

import (
	"fmt"
	"io"
	"log"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"strings"
)

// ReportType identifies a financial report.
type ReportType string

const (
	ReportBalanceSheet ReportType = "balancesheet"
	ReportProfitLoss   ReportType = "profitloss"
)

type reportDef struct {
	page     string
	selector string
}

var reportDefs = map[ReportType]reportDef{
	ReportBalanceSheet: {page: "FinBalanceSheetClient.aspx", selector: "View:tBalanceSheet"},
	ReportProfitLoss:   {page: "FinBalanceSheetClient.aspx", selector: "View:tProfitLoss"},
}

type formTokens struct {
	CSRFToken          string
	ViewState          string
	ViewStateGenerator string
}

func extractFormTokens(html string) (formTokens, error) {
	csrf := extractAttr(html, `name="CSRFToken"[^>]*value="([^"]+)"`)
	vs := extractAttr(html, `name="__VIEWSTATE"[^>]*value="([^"]+)"`)
	vsgen := extractAttr(html, `name="__VIEWSTATEGENERATOR"[^>]*value="([^"]+)"`)
	if csrf == "" || vs == "" || vsgen == "" {
		return formTokens{}, fmt.Errorf("missing form tokens (csrf=%q, viewstate=%d bytes, vsgen=%q)", csrf, len(vs), vsgen)
	}
	return formTokens{CSRFToken: csrf, ViewState: vs, ViewStateGenerator: vsgen}, nil
}

func buildSmartFilter(year, from, to int) string {
	return fmt.Sprintf(`{"outputArr":["Period","Other","%d","%d","%d"],"selectedContentType":"Range"}`, year, from, to)
}

// DownloadReport downloads a financial report as Excel (.xlsx).
// Returns the file bytes, suggested filename, and any error.
func (c *Client) DownloadReport(rt ReportType, year, fromPeriod, toPeriod int) ([]byte, string, error) {
	def, ok := reportDefs[rt]
	if !ok {
		return nil, "", fmt.Errorf("unknown report type: %s", rt)
	}

	smartFilter := buildSmartFilter(year, fromPeriod, toPeriod)
	compareTo := `{"outputArr":["Period","Previous"]}`

	// Step 1: GET report page to extract form tokens
	pageParams := url.Values{
		"ShowReviewedPeriod":   {"True"},
		"SmartFilter":          {smartFilter},
		"SmartFilterCompareTo": {compareTo},
		"Selector":             {def.selector},
		"_Division_":           {c.divisionID},
	}
	pageURL := c.url() + "/docs/" + def.page + "?" + pageParams.Encode()
	log.Printf("GET report page: %s", def.page)

	resp, err := c.http.Get(pageURL)
	if err != nil {
		return nil, "", fmt.Errorf("GET report page: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("report page returned %d", resp.StatusCode)
	}

	tokens, err := extractFormTokens(string(body))
	if err != nil {
		return nil, "", fmt.Errorf("extract form tokens: %w", err)
	}
	log.Println("Extracted form tokens")

	// Step 2: Initialize export
	initParams := url.Values{
		"ExportType": {"1"},
		"PageName":   {def.page},
		"_Division_": {c.divisionID},
	}
	initURL := c.url() + "/docs/SysExportInitialize.aspx?" + initParams.Encode()

	resp, err = c.http.Post(initURL, "", nil)
	if err != nil {
		return nil, "", fmt.Errorf("POST SysExportInitialize: %w", err)
	}
	_ = resp.Body.Close()

	exportMessageID := resp.Header.Get("exportmessageid")
	if exportMessageID == "" {
		return nil, "", fmt.Errorf("no exportmessageid in SysExportInitialize response")
	}
	log.Printf("Export initialized: %s", exportMessageID)

	// Step 3: POST to download the Excel file
	exportParams := make(url.Values)
	maps.Copy(exportParams, pageParams)
	exportParams.Set("SysDoPrinting", "1")
	exportParams.Set("SysExporting", "1")
	exportParams.Set("ExportMessageId", exportMessageID)
	exportParams.Set("ExportPageName", def.page)
	exportParams.Set("IsNewExport", "true")

	exportURL := c.url() + "/docs/" + def.page + "?" + exportParams.Encode()

	formData := url.Values{
		"CSRFToken":               {tokens.CSRFToken},
		"__VIEWSTATE":             {tokens.ViewState},
		"__VIEWSTATEGENERATOR":    {tokens.ViewStateGenerator},
		"_Division_":              {c.divisionID},
		"SmartFilter":             {smartFilter},
		"SmartFilterCompareTo":    {compareTo},
		"ShowReviewedPeriod":      {"True"},
		"Selector_tabs":           {def.selector},
		"SysNoBack":               {"1"},
		"PageInWidget":            {"False"},
		"Factor":                  {"1.00"},
		"SplitPeriods":            {"1"},
		"SmartFilterYears":        {fmt.Sprintf("%d", year)},
		"SmartFilterPeriodYears":  {fmt.Sprintf("%d", year)},
		"SmartFilterPeriods$From": {fmt.Sprintf("%d", fromPeriod)},
		"SmartFilterPeriods$To":   {fmt.Sprintf("%d", toPeriod)},
	}

	req, err := http.NewRequest("POST", exportURL, strings.NewReader(formData.Encode()))
	if err != nil {
		return nil, "", fmt.Errorf("build export request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("isexporting", "1")

	resp, err = c.http.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("POST export: %w", err)
	}
	xlsxData, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("export returned %d: %s", resp.StatusCode, string(xlsxData[:min(200, len(xlsxData))]))
	}

	filename := parseContentDisposition(resp.Header.Get("Content-Disposition"))
	log.Printf("Downloaded %d bytes, filename=%q", len(xlsxData), filename)

	// Step 4: Cleanup (fire-and-forget)
	cleanupParams := url.Values{
		"ExportType":      {"1"},
		"ExportMessageId": {exportMessageID},
		"_Division_":      {c.divisionID},
	}
	cleanupURL := c.url() + "/docs/SysExportCleanUp.aspx?" + cleanupParams.Encode()
	resp, err = c.http.Post(cleanupURL, "", nil)
	if err != nil {
		log.Printf("Warning: export cleanup failed: %v", err)
	} else {
		_ = resp.Body.Close()
	}

	return xlsxData, filename, nil
}

func parseContentDisposition(header string) string {
	if header == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(header)
	if err != nil {
		return ""
	}
	return params["filename"]
}
