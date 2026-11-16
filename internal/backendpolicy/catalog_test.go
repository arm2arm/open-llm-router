package backendpolicy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arm2arm/open-llm-router/internal/config"
)

func catalogPolicy(t *testing.T, origins ...string) *Policy {
	t.Helper()
	model := config.Model{}
	for _, origin := range origins {
		model.Pool = append(model.Pool, config.Replica{URL: origin})
	}
	p, err := New(&config.Config{Models: map[string]config.Model{"m": model}}, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCatalogRefusesUnapprovedDestinations(t *testing.T) {
	p := catalogPolicy(t)
	for _, origin := range []string{
		"http://169.254.169.254", "http://[::ffff:169.254.169.254]", "http://100.100.100.200",
		"http://[fd00:ec2::254]", "http://127.0.0.1", "http://[::1]", "http://0.0.0.0",
		"http://240.0.0.1", "http://user:secret@8.8.8.8", "http://8.8.8.8/control",
		"http://8.8.8.8?key=secret", "http://8.8.8.8#fragment", "file:///etc/passwd", "http://8.8.8.8:0",
	} {
		for _, private := range []bool{false, true} {
			if client, err := p.NewCatalogClient(t.Context(), origin, private); err == nil || client != nil {
				t.Fatalf("unsafe catalog destination accepted: %s private=%t", origin, private)
			}
		}
	}
}

func TestCatalogAllowsExplicitPrivateHTTP(t *testing.T) {
	p, err := New(&config.Config{Security: config.Security{RequireTLS: true}}, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	for _, origin := range []string{"http://10.1.2.3:8000", "http://[fd12::1]:11434", "http://141.33.4.201:8081"} {
		if _, err := p.NewCatalogClient(t.Context(), origin, false); err == nil {
			t.Fatal("private HTTP did not require administrator opt-in", origin)
		}
		client, err := p.NewCatalogClient(t.Context(), origin, true)
		if err != nil {
			t.Fatal("explicit private HTTP backend refused", origin, err)
		}
		client.Close()
	}
}

func TestCatalogPinsDestinationAndScopesResources(t *testing.T) {
	var calls, proxyCalls atomic.Int32
	var authority string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Host != authority || r.Header.Get("Authorization") != "Bearer catalog-test-key" || r.URL.Path != "/v1/models" {
			t.Error("catalog lost its authority, resource or origin-bound key", r.Host, r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer backend.Close()
	origin := strings.Replace(backend.URL, "127.0.0.1", "localhost", 1)
	authority = strings.TrimPrefix(origin, "http://")
	proxy := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { proxyCalls.Add(1) }))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	client, err := catalogPolicy(t, origin).NewCatalogClient(t.Context(), origin, false)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	host, _, err := net.SplitHostPort(client.target.Host)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := netip.ParseAddr(host); err != nil {
		t.Fatal("catalog URL still contains a hostname instead of a pinned IP", err)
	}
	if _, err := client.transport.DialContext(t.Context(), "tcp", "169.254.169.254:80"); err == nil {
		t.Fatal("catalog transport dialed a different destination")
	}
	for _, path := range []string{"/control", "//169.254.169.254/", "/v1/models?target=metadata", "/v1/models/../control"} {
		if _, err := client.Get(t.Context(), path, "catalog-test-key"); err == nil {
			t.Fatal("arbitrary catalog resource accepted", path)
		}
	}
	resp, err := client.Get(t.Context(), "/v1/models", "catalog-test-key")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || calls.Load() != 1 || proxyCalls.Load() != 0 {
		t.Fatal("catalog request escaped its scope", resp.StatusCode, calls.Load(), proxyCalls.Load())
	}
	if _, err := new(CatalogClient).Get(t.Context(), "/v1/models", ""); err == nil {
		t.Fatal("uninitialized catalog capability accepted")
	}
}

func TestCatalogDoesNotFollowRedirectsOrForwardKeys(t *testing.T) {
	var escaped atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { escaped.Add(1) }))
	defer other.Close()
	for _, status := range []int{302, 307} {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, other.URL+"/control", status)
		}))
		client, err := catalogPolicy(t, backend.URL).NewCatalogClient(t.Context(), backend.URL, false)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Get(t.Context(), "/v1/models", "catalog-test-key")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		client.Close()
		backend.Close()
		if resp.StatusCode != status || escaped.Load() != 0 {
			t.Fatal("catalog followed a redirect or forwarded its key", resp.StatusCode, escaped.Load())
		}
	}
}

func TestCatalogTLSVerifiesOriginalHostname(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS.ServerName != "localhost" || !strings.HasPrefix(r.Host, "localhost:") {
			t.Error("TLS SNI or HTTP Host lost the original hostname")
		}
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	backend.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	backend.StartTLS()
	defer backend.Close()
	original := http.DefaultTransport
	transport := original.(*http.Transport).Clone()
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	transport.TLSClientConfig = &tls.Config{RootCAs: roots}
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = original; transport.CloseIdleConnections() }()
	origin := strings.Replace(backend.URL, "127.0.0.1", "localhost", 1)
	p := catalogPolicy(t, origin, backend.URL)
	client, err := p.NewCatalogClient(t.Context(), origin, false)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	resp, err := client.Get(t.Context(), "/v1/models", "")
	if err != nil {
		t.Fatal("TLS did not verify the original hostname against its certificate", err)
	}
	resp.Body.Close()
	// The same trusted certificate must fail when the configured hostname is
	// its IP address: trusting a CA must not disable hostname verification.
	wrong, err := p.NewCatalogClient(t.Context(), backend.URL, false)
	if err != nil {
		t.Fatal(err)
	}
	defer wrong.Close()
	if resp, err := wrong.Get(t.Context(), "/v1/models", ""); err == nil {
		resp.Body.Close()
		t.Fatal("certificate hostname mismatch was accepted")
	}
}
