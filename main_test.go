package main

import (
	"math"
	"net/netip"
	"slices"
	"strconv"
	"testing"
)

func TestParseNameservers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  []NSConfig
	}{
		{
			name:  "out-of-zone NS",
			input: "ns.example.org",
			want:  []NSConfig{{FQDN: "ns.example.org."}},
		},
		{
			name:  "out-of-zone NS with IPv4 glue",
			input: "ns.example.org.:1.2.3.4",
			want:  []NSConfig{{FQDN: "ns.example.org.", Glue: []netip.Addr{netip.MustParseAddr("1.2.3.4")}}},
		},
		{
			name:  "out-of-zone NS with IPv4 and IPv6 glue",
			input: "ns.example.org.:1.2.3.4,2001:db8::1",
			want: []NSConfig{{FQDN: "ns.example.org.", Glue: []netip.Addr{
				netip.MustParseAddr("1.2.3.4"), netip.MustParseAddr("2001:db8::1"),
			}}},
		},
		{
			name:  "in-zone NS with IPv4 glue",
			input: "ns.example.com.:1.2.3.4",
			want:  []NSConfig{{FQDN: "ns.example.com.", Glue: []netip.Addr{netip.MustParseAddr("1.2.3.4")}}},
		},
		{
			name:  "multiple nameservers",
			input: "ns1.example.com:1.2.3.4;ns2.example.org.",
			want: []NSConfig{
				{FQDN: "ns1.example.com.", Glue: []netip.Addr{netip.MustParseAddr("1.2.3.4")}},
				{FQDN: "ns2.example.org."},
			},
		},
		{
			name:  "normalizes to lowercase and adds trailing dot",
			input: "NS.Example.ORG",
			want:  []NSConfig{{FQDN: "ns.example.org."}},
		},
		{
			name:  "trims whitespace",
			input: " ns.example.org.  : 1.2.3.4 ,   5.6.7.8  ",
			want: []NSConfig{{FQDN: "ns.example.org.", Glue: []netip.Addr{
				netip.MustParseAddr("1.2.3.4"), netip.MustParseAddr("5.6.7.8"),
			}}},
		},
	}

	errTests := []struct {
		name  string
		input string
	}{
		{
			name:  "empty string",
			input: "",
		},
		{
			name:  "in-zone NS without glue",
			input: "ns.example.com.",
		},
		{
			name:  "invalid glue IP",
			input: "ns.example.org:stuff",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			nameservers, err := parseNameservers(test.input, "example.com.")
			if err != nil {
				t.Fatalf("parsing nameservers: %v", err)
			}

			if len(nameservers) != len(test.want) {
				t.Fatalf("invalid number of nameservers: got %v, expected %v", len(nameservers), len(test.want))
			}

			for i, ns := range nameservers {
				if ns.FQDN != test.want[i].FQDN {
					t.Errorf("invalid FQDN of nameserver #%v: got %v, want %v", i, ns.FQDN, test.want[i].FQDN)
				}

				if !slices.Equal(ns.Glue, test.want[i].Glue) {
					t.Errorf("invalid glue of nameserver #%v: got %v, want %v", i, ns.Glue, test.want[i].Glue)
				}
			}
		})
	}

	for _, test := range errTests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := parseNameservers(test.input, "example.com.")
			if err == nil {
				t.Fatal("expected error while parsing nameservers but got no error")
			}
		})
	}
}

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
