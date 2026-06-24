package exactonline

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBuildSmartFilter(t *testing.T) {
	tests := []struct {
		year, from, to int
		want           string
	}{
		{2026, 1, 12, `{"outputArr":["Period","Other","2026","1","12"],"selectedContentType":"Range"}`},
		{2025, 3, 6, `{"outputArr":["Period","Other","2025","3","6"],"selectedContentType":"Range"}`},
	}
	for _, tt := range tests {
		got := buildSmartFilter(tt.year, tt.from, tt.to)
		if got != tt.want {
			t.Errorf("buildSmartFilter(%d,%d,%d) = %q, want %q", tt.year, tt.from, tt.to, got, tt.want)
		}
	}
}

func TestExtractFormTokens(t *testing.T) {
	html := `<html>
<input type="hidden" name="CSRFToken" id="CSRFToken" value="abc123_token-value" />
<input type="hidden" name="__VIEWSTATE" id="__VIEWSTATE" value="viewstate_data==" />
<input type="hidden" name="__VIEWSTATEGENERATOR" id="__VIEWSTATEGENERATOR" value="F4FE2432" />
</html>`

	tokens, err := extractFormTokens(html)
	if err != nil {
		t.Fatal(err)
	}
	if tokens.CSRFToken != "abc123_token-value" {
		t.Errorf("CSRFToken = %q, want %q", tokens.CSRFToken, "abc123_token-value")
	}
	if tokens.ViewState != "viewstate_data==" {
		t.Errorf("ViewState = %q, want %q", tokens.ViewState, "viewstate_data==")
	}
	if tokens.ViewStateGenerator != "F4FE2432" {
		t.Errorf("ViewStateGenerator = %q, want %q", tokens.ViewStateGenerator, "F4FE2432")
	}
}

func TestExtractFormTokensMissing(t *testing.T) {
	html := `<html><body>no tokens here</body></html>`
	_, err := extractFormTokens(html)
	if err == nil {
		t.Error("expected error for missing tokens")
	}
}

func TestDownloadReport(t *testing.T) {
	const (
		fakeCSRF      = "test-csrf-token"
		fakeViewState = "test-viewstate"
		fakeVSGen     = "DEADBEEF"
		fakeExportID  = "aaaabbbb-cccc-dddd-eeee-ffffffffffff"
		fakeXLSX      = "PK\x03\x04fake-xlsx-content"
		fakeFilename  = "TestCompany-report.xlsx"
	)

	step := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		// Step 1: GET report page
		case r.Method == "GET" && strings.Contains(r.URL.Path, "FinBalanceSheetClient.aspx"):
			step = 1
			_, _ = fmt.Fprintf(w, `<html>
<input type="hidden" name="CSRFToken" id="CSRFToken" value="%s" />
<input type="hidden" name="__VIEWSTATE" id="__VIEWSTATE" value="%s" />
<input type="hidden" name="__VIEWSTATEGENERATOR" id="__VIEWSTATEGENERATOR" value="%s" />
</html>`, fakeCSRF, fakeViewState, fakeVSGen)

		// Step 2: POST SysExportInitialize
		case r.Method == "POST" && strings.Contains(r.URL.Path, "SysExportInitialize"):
			if step != 1 {
				t.Errorf("SysExportInitialize called out of order (step=%d)", step)
			}
			step = 2
			w.Header().Set("exportmessageid", fakeExportID)
			w.Header().Set("isnewexport", "true")
			w.WriteHeader(200)

		// Step 3: POST report page (export)
		case r.Method == "POST" && strings.Contains(r.URL.Path, "FinBalanceSheetClient.aspx"):
			if step != 2 {
				t.Errorf("export POST called out of order (step=%d)", step)
			}
			step = 3

			// Verify export params in query string
			if r.URL.Query().Get("SysExporting") != "1" {
				t.Error("missing SysExporting=1")
			}
			if r.URL.Query().Get("ExportMessageId") != fakeExportID {
				t.Errorf("ExportMessageId = %q, want %q", r.URL.Query().Get("ExportMessageId"), fakeExportID)
			}

			// Verify form body contains CSRF token
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), "CSRFToken="+fakeCSRF) {
				t.Error("request body missing CSRFToken")
			}

			w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", fakeFilename))
			_, _ = w.Write([]byte(fakeXLSX))

		// Step 4: POST SysExportCleanUp
		case r.Method == "POST" && strings.Contains(r.URL.Path, "SysExportCleanUp"):
			if step != 3 {
				t.Errorf("SysExportCleanUp called out of order (step=%d)", step)
			}
			step = 4
			if r.URL.Query().Get("ExportMessageId") != fakeExportID {
				t.Errorf("cleanup ExportMessageId = %q, want %q", r.URL.Query().Get("ExportMessageId"), fakeExportID)
			}
			w.WriteHeader(200)

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	c := newTestClient(srv)

	data, filename, err := c.DownloadReport(ReportProfitLoss, 2026, 1, 12)
	if err != nil {
		t.Fatal(err)
	}
	if step != 4 {
		t.Errorf("expected 4 steps, got %d", step)
	}
	if string(data) != fakeXLSX {
		t.Errorf("data = %q, want %q", string(data), fakeXLSX)
	}
	if filename != fakeFilename {
		t.Errorf("filename = %q, want %q", filename, fakeFilename)
	}
}

func newTestClient(srv *httptest.Server) *Client {
	return &Client{
		http:       srv.Client(),
		divisionID: "1234567",
		baseURL:    srv.URL,
	}
}
