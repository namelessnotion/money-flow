package ledger

import "testing"

// TestResolveClusterAddress_PassesNumericAddressThrough: an address that is
// already numeric must be handed to TigerBeetle untouched, including IPv6.
func TestResolveClusterAddress_PassesNumericAddressThrough(t *testing.T) {
	t.Parallel()
	for _, addr := range []string{"127.0.0.1:3000", "172.18.0.2:3000", "[::1]:3000"} {
		got, err := resolveClusterAddress(addr)
		if err != nil {
			t.Errorf("resolveClusterAddress(%q) error = %v, want nil", addr, err)
			continue
		}
		if got != addr {
			t.Errorf("resolveClusterAddress(%q) = %q, want it unchanged", addr, got)
		}
	}
}

// TestResolveClusterAddress_ResolvesHostname is the case that matters in
// Docker: a Compose service name is a DNS name, and tb_client only accepts
// numeric addresses. localhost is used because it resolves without a network.
func TestResolveClusterAddress_ResolvesHostname(t *testing.T) {
	t.Parallel()
	got, err := resolveClusterAddress("localhost:3000")
	if err != nil {
		t.Fatalf("resolveClusterAddress() error = %v, want nil", err)
	}
	if got == "localhost:3000" {
		t.Fatal("resolveClusterAddress() returned the hostname unchanged, want a numeric address")
	}
	if got != "127.0.0.1:3000" && got != "[::1]:3000" {
		t.Errorf("resolveClusterAddress() = %q, want localhost's numeric form", got)
	}
}

// TestResolveClusterAddress_PrefersIPv4 keeps the returned address in the form
// TigerBeetle is routinely deployed with when a host offers both.
func TestResolveClusterAddress_PrefersIPv4(t *testing.T) {
	t.Parallel()
	got, err := resolveClusterAddress("localhost:3000")
	if err != nil {
		t.Fatalf("resolveClusterAddress() error = %v, want nil", err)
	}
	// Only assert the preference when the resolver actually offered both.
	if hasBothFamilies(t, "localhost") && got != "127.0.0.1:3000" {
		t.Errorf("resolveClusterAddress() = %q, want the IPv4 form when both are available", got)
	}
}

func TestResolveClusterAddress_RejectsMissingPort(t *testing.T) {
	t.Parallel()
	if _, err := resolveClusterAddress("tigerbeetle"); err == nil {
		t.Fatal("resolveClusterAddress() error = nil, want an error for an address with no port")
	}
}

func TestResolveClusterAddress_ReportsUnresolvableHost(t *testing.T) {
	t.Parallel()
	if _, err := resolveClusterAddress("no-such-host.invalid:3000"); err == nil {
		t.Fatal("resolveClusterAddress() error = nil, want an error naming the unresolvable host")
	}
}

// hasBothFamilies reports whether host resolves to at least one IPv4 and one
// IPv6 address, so the preference assertion only runs where it is meaningful.
func hasBothFamilies(t *testing.T, host string) bool {
	t.Helper()
	ips, err := netLookupIP(host)
	if err != nil {
		return false
	}
	var v4, v6 bool
	for _, ip := range ips {
		if ip.To4() != nil {
			v4 = true
		} else {
			v6 = true
		}
	}
	return v4 && v6
}
