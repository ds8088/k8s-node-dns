package main

import (
	"math"
	"strconv"
	"testing"
)

func TestToUint32Saturate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input uint
		want  uint32
	}{
		{0, 0},
		{1, 1},
		{math.MaxUint32, math.MaxUint32},
		{math.MaxUint32 + 1, math.MaxUint32},
		{math.MaxUint, math.MaxUint32},
	}

	for _, test := range tests {
		t.Run(strconv.FormatUint(uint64(test.input), 10), func(t *testing.T) {
			t.Parallel()

			got := toUint32Saturate(&test.input)
			if got != test.want {
				t.Errorf("invalid result of toUint32Saturate for %v: got %v, expected %v", test.input, got, test.want)
			}
		})
	}
}

func TestToEmail(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{"admin@example.com", "admin.example.com."},
		{"admin@example.com.", "admin.example.com."},
		{"deep.subdomain@example.com", `deep\.subdomain.example.com.`},
		{"a.b.c@example.com", `a\.b\.c.example.com.`},
		{"stuff", "stuff."},
	}

	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			t.Parallel()

			got := toEmail(test.input)
			if got != test.want {
				t.Errorf("invalid email transform for %v: got %v, expected %v", test.input, got, test.want)
			}
		})
	}
}

func TestToFQDN(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{"example.com", "example.com."},
		{"example.com.", "example.com."},
		{"a", "a."},
		{".", "."},
	}

	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			t.Parallel()

			got := toFQDN(test.input)
			if got != test.want {
				t.Errorf("invalid FQDN transform for %v: got %v, expected %v", test.input, got, test.want)
			}
		})
	}
}
