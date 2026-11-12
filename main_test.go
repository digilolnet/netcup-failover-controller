package main

import (
	"testing"
	"time"
)

func TestResyncInterval(t *testing.T) {
	tests := []struct {
		name    string
		env     string
		want    time.Duration
		wantErr bool
	}{
		{name: "unset uses the default", env: "", want: defaultResyncInterval},
		{name: "explicit duration", env: "10m", want: 10 * time.Minute},
		{name: "zero disables", env: "0", want: 0},
		{name: "not a duration", env: "five minutes", wantErr: true},
		{name: "negative", env: "-1m", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("RESYNC_INTERVAL", tt.env)
			got, err := resyncInterval()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
			if got != tt.want {
				t.Errorf("resyncInterval = %v, want %v", got, tt.want)
			}
		})
	}
}
