package app

import "testing"

func TestLoadConfig(t *testing.T) {
	tests := []struct {
		name           string
		args           []string
		wantAddr       string
		wantBrowserURL string
		wantNoBrowser  bool
		wantErr        bool
	}{
		{
			name:           "defaults",
			args:           nil,
			wantAddr:       "127.0.0.1:3000",
			wantBrowserURL: "http://localhost:3000", // loopback shown as localhost
		},
		{
			name:           "custom port",
			args:           []string{"--port", "8080"},
			wantAddr:       "127.0.0.1:8080",
			wantBrowserURL: "http://localhost:8080",
		},
		{
			name:           "custom host shows real bind address",
			args:           []string{"--host", "0.0.0.0", "--port", "9000"},
			wantAddr:       "0.0.0.0:9000",
			wantBrowserURL: "http://0.0.0.0:9000",
		},
		{
			name:          "no-browser flag",
			args:          []string{"--no-browser"},
			wantAddr:      "127.0.0.1:3000",
			wantNoBrowser: true,
		},
		{
			name:    "unknown flag errors",
			args:    []string{"--bogus"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := LoadConfig(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("LoadConfig(%v): want error, got nil", tt.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadConfig(%v): unexpected error: %v", tt.args, err)
			}
			if cfg.Addr != tt.wantAddr {
				t.Errorf("Addr = %q, want %q", cfg.Addr, tt.wantAddr)
			}
			if tt.wantBrowserURL != "" && cfg.BrowserURL != tt.wantBrowserURL {
				t.Errorf("BrowserURL = %q, want %q", cfg.BrowserURL, tt.wantBrowserURL)
			}
			if cfg.NoBrowser != tt.wantNoBrowser {
				t.Errorf("NoBrowser = %v, want %v", cfg.NoBrowser, tt.wantNoBrowser)
			}
		})
	}
}
