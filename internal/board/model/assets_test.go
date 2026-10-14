package model

import (
	"strings"
	"testing"
)

func TestNormalizeAssetEquivalentAddresses(t *testing.T) {
	for _, pair := range [][2]AssetSpec{
		{{Kind: "host", Value: "EXAMPLE.COM."}, {Kind: "host", Value: "example.com"}},
		{{Kind: "host", Value: "2001:0db8:0:0:0:0:0:1"}, {Kind: "host", Value: "2001:db8::1"}},
		{{Kind: "service", Value: "HTTP://EXAMPLE.COM.:080/"}, {Kind: "service", Value: "http://example.com"}},
		{{Kind: "service", Value: "https://[2001:0db8::1]:443/"}, {Kind: "service", Value: "https://[2001:db8::1]"}},
		{{Kind: "service", Value: "tcp://EXAMPLE.COM:0022"}, {Kind: "service", Value: "tcp://example.com:22"}},
		{{Kind: "endpoint", Value: "https://EXAMPLE.COM.:443", Method: "GET"}, {Kind: "endpoint", Value: "https://example.com/", Method: "GET"}},
	} {
		a, err := NormalizeAsset(pair[0])
		if err != nil {
			t.Fatal(err)
		}
		b, err := NormalizeAsset(pair[1])
		if err != nil || a != b || !strings.HasPrefix(a.ID, "asset_") || len(a.ID) != 38 {
			t.Fatalf("equivalent addresses differ: %+v %+v (%v)", a, b, err)
		}
	}
}

func TestNormalizeAssetPreservesMethodPathAndQueryIdentity(t *testing.T) {
	seen := map[string]Asset{}
	for _, spec := range []AssetSpec{
		{Kind: "host", Value: "example.com"},
		{Kind: "service", Value: "https://example.com"},
		{Kind: "endpoint", Value: "https://example.com/Orders?a=1&b=2", Method: "GET"},
		{Kind: "endpoint", Value: "https://example.com/orders?a=1&b=2", Method: "GET"},
		{Kind: "endpoint", Value: "https://example.com/Orders?b=2&a=1", Method: "GET"},
		{Kind: "endpoint", Value: "https://example.com/Orders?a=2&b=2", Method: "GET"},
		{Kind: "endpoint", Value: "https://example.com/Orders?a=1&b=2", Method: "POST"},
		// HTTP method tokens are case sensitive, including extension methods.
		{Kind: "endpoint", Value: "https://example.com/Orders?a=1&b=2", Method: "get"},
		{Kind: "endpoint", Value: "https://example.com/%4frders?a=1&b=2", Method: "GET"},
		{Kind: "endpoint", Value: "https://example.com/a%2fb", Method: "GET"},
		{Kind: "endpoint", Value: "https://example.com/a/b", Method: "GET"},
		{Kind: "endpoint", Value: "https://example.com/", Method: "GET"},
		{Kind: "endpoint", Value: "https://example.com/?", Method: "GET"},
	} {
		asset, err := NormalizeAsset(spec)
		if err != nil {
			t.Fatalf("%+v: %v", spec, err)
		}
		if previous, ok := seen[asset.ID]; ok {
			t.Fatalf("distinct targets merged: %+v and %+v", previous, asset)
		}
		seen[asset.ID] = asset
	}
}

func TestNormalizeAssetRejectsAmbiguousInputs(t *testing.T) {
	for _, spec := range []AssetSpec{
		{Kind: "company", Value: "example.com"},
		{Kind: "host", Value: "https://example.com"},
		{Kind: "host", Value: "example.com:443"},
		{Kind: "host", Value: "999.1.1.1"},
		{Kind: "host", Value: "127.0.0.01"},
		{Kind: "host", Value: "example..com"},
		{Kind: "host", Value: "a.-b.example"},
		{Kind: "host", Value: "fe80::1%eth0"},
		{Kind: "host", Value: "example.com", Method: "GET"},
		{Kind: "service", Value: "https://example.com/path"},
		{Kind: "service", Value: "https://example.com/?q=1"},
		{Kind: "service", Value: "https://example.com?"},
		{Kind: "service", Value: "tcp://example.com"},
		{Kind: "service", Value: "http://2001:db8::1"},
		{Kind: "service", Value: "http://[example.com]"},
		{Kind: "service", Value: "http://[127.0.0.1]"},
		{Kind: "service", Value: "http://example.com:"},
		{Kind: "service", Value: "http://example.com:0"},
		{Kind: "service", Value: "http://example.com:65536"},
		{Kind: "endpoint", Value: "http://example.com/"},
		{Kind: "endpoint", Value: "ftp://example.com/", Method: "GET"},
		{Kind: "endpoint", Value: "http://user:secret@example.com/", Method: "GET"},
		{Kind: "endpoint", Value: "http://example.com/#", Method: "GET"},
		{Kind: "endpoint", Value: "http://example.com/", Method: "GET POST"},
	} {
		if _, err := NormalizeAsset(spec); err == nil {
			t.Fatalf("accepted invalid asset: %+v", spec)
		}
	}
}
