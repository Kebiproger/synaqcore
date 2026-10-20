package config

import (
	"testing"
	"time"
)

func TestString(t *testing.T) {
	t.Setenv("SYNAQ_TEST_STR", "")
	if got := String("SYNAQ_TEST_STR", "def"); got != "def" {
		t.Errorf("пустое значение: got %q, want def", got)
	}
	t.Setenv("SYNAQ_TEST_STR", "val")
	if got := String("SYNAQ_TEST_STR", "def"); got != "val" {
		t.Errorf("заданное значение: got %q, want val", got)
	}
}

func TestInt(t *testing.T) {
	tests := []struct {
		raw     string
		want    int
		wantErr bool
	}{
		{raw: "", want: 7},
		{raw: "42", want: 42},
		{raw: "0", wantErr: true},
		{raw: "-1", wantErr: true},
		{raw: "abc", wantErr: true},
		{raw: "1.5", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			t.Setenv("SYNAQ_TEST_INT", tt.raw)
			got, err := Int("SYNAQ_TEST_INT", 7)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestDuration(t *testing.T) {
	tests := []struct {
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{raw: "", want: time.Second},
		{raw: "500ms", want: 500 * time.Millisecond},
		{raw: "2s", want: 2 * time.Second},
		{raw: "0s", wantErr: true},
		{raw: "-1s", wantErr: true},
		{raw: "10", wantErr: true},
		{raw: "abc", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			t.Setenv("SYNAQ_TEST_DUR", tt.raw)
			got, err := Duration("SYNAQ_TEST_DUR", time.Second)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMustValid(t *testing.T) {
	t.Setenv("SYNAQ_TEST_INT", "3")
	t.Setenv("SYNAQ_TEST_DUR", "3s")
	if got := MustInt("SYNAQ_TEST_INT", 1); got != 3 {
		t.Errorf("MustInt = %d, want 3", got)
	}
	if got := MustDuration("SYNAQ_TEST_DUR", time.Second); got != 3*time.Second {
		t.Errorf("MustDuration = %v, want 3s", got)
	}
}
