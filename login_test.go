package exactonline

import (
	"testing"
)

func TestFillB2CFields(t *testing.T) {
	const (
		user   = "user@example.com"
		pass   = "s3cr3t"
		secret = "JBSWY3DPEHPK3PXP" // valid base32 TOTP secret
	)

	tests := []struct {
		name    string
		fields  []string
		want    map[string]string // expected non-empty fields (value "" means "just present & non-empty")
		wantErr bool
	}{
		{
			name:   "combined email+password page",
			fields: []string{"signInName", "password"},
			want:   map[string]string{"signInName": user, "password": pass, "request_type": "RESPONSE"},
		},
		{
			name:   "email-only page (split flow)",
			fields: []string{"signInName"},
			want:   map[string]string{"signInName": user, "request_type": "RESPONSE"},
		},
		{
			name:   "password-only page (split flow)",
			fields: []string{"password"},
			want:   map[string]string{"password": pass, "request_type": "RESPONSE"},
		},
		{
			name:   "user agent page",
			fields: []string{"userAgent"},
			want:   map[string]string{"userAgent": userAgent, "request_type": "RESPONSE"},
		},
		{
			name:   "totp page",
			fields: []string{"rememberMePeriodChangeInfo", "totpVerificationCode", "reset_totp", "totp_skip_days"},
			want:   map[string]string{"totpVerificationCode": "", "reset_totp": "false", "totp_skip_days": "true", "request_type": "RESPONSE"},
		},
		{
			name:   "exact assistant payload is optional",
			fields: []string{"signInName", "password", "exactAssistantPayload"},
			want:   map[string]string{"signInName": user, "password": pass, "request_type": "RESPONSE"},
		},
		{
			name:    "unknown required fields",
			fields:  []string{"someBrandNewField"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := fillB2CFields(tt.fields, user, pass, secret)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (data=%v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			for k, v := range tt.want {
				gv := got.Get(k)
				if v == "" {
					if gv == "" {
						t.Errorf("field %q: expected non-empty value", k)
					}
				} else if gv != v {
					t.Errorf("field %q = %q, want %q", k, gv, v)
				}
			}
		})
	}
}
