package v2

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/ReCasaOS/CasaOS-AppManagement/codegen"
	"gotest.tools/v3/assert"
)

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	assert.NilError(t, err)
	return parsed
}

func TestFrameBlocked(t *testing.T) {
	const dashboard = "http://192.168.10.3"
	const app = "https://192.168.10.3:8443/nifi/"

	for _, c := range []struct {
		name    string
		headers map[string][]string
		page    string
		origin  string
		reason  codegen.WebFrameCheckReason
	}{
		{"nothing said", nil, app, dashboard, ""},
		// NiFi, which is how this was found
		{"nifi", map[string][]string{"X-Frame-Options": {"SAMEORIGIN"}, "Content-Security-Policy": {"frame-ancestors 'self'"}}, app, dashboard, codegen.FrameAncestors},
		{"sameorigin, other port", map[string][]string{"X-Frame-Options": {"SAMEORIGIN"}}, app, dashboard, codegen.XFrameOptions},
		{"sameorigin, same origin", map[string][]string{"X-Frame-Options": {"sameorigin"}}, "http://192.168.10.3/app/", dashboard, ""},
		{"deny", map[string][]string{"X-Frame-Options": {"DENY"}}, app, dashboard, codegen.XFrameOptions},
		{"allow-from is ignored", map[string][]string{"X-Frame-Options": {"ALLOW-FROM http://example.com"}}, app, dashboard, ""},
		// a browser ignores X-Frame-Options once frame-ancestors speaks
		{"frame-ancestors overrides xfo", map[string][]string{"X-Frame-Options": {"DENY"}, "Content-Security-Policy": {"default-src 'self'; frame-ancestors *"}}, app, dashboard, ""},
		{"csp without frame-ancestors leaves xfo", map[string][]string{"X-Frame-Options": {"DENY"}, "Content-Security-Policy": {"default-src 'self'"}}, app, dashboard, codegen.XFrameOptions},
		{"none", map[string][]string{"Content-Security-Policy": {"frame-ancestors 'none'"}}, app, dashboard, codegen.FrameAncestors},
		{"host source", map[string][]string{"Content-Security-Policy": {"frame-ancestors http://192.168.10.3"}}, app, dashboard, ""},
		{"host source, wrong port", map[string][]string{"Content-Security-Policy": {"frame-ancestors http://192.168.10.3:8080"}}, app, dashboard, codegen.FrameAncestors},
		{"host source, any port", map[string][]string{"Content-Security-Policy": {"frame-ancestors http://192.168.10.3:*"}}, app, "http://192.168.10.3:8080", ""},
		{"host source without scheme takes the page's", map[string][]string{"Content-Security-Policy": {"frame-ancestors 192.168.10.3"}}, app, dashboard, codegen.FrameAncestors},
		{"wildcard host", map[string][]string{"Content-Security-Policy": {"frame-ancestors https://*.home.lan"}}, app, "https://casa.home.lan", ""},
		{"scheme source", map[string][]string{"Content-Security-Policy": {"frame-ancestors http:"}}, app, "https://casa.home.lan", ""},
		{"every policy must allow", map[string][]string{"Content-Security-Policy": {"frame-ancestors *", "frame-ancestors 'self'"}}, app, dashboard, codegen.FrameAncestors},
		{"report-only does not block", map[string][]string{"Content-Security-Policy-Report-Only": {"frame-ancestors 'none'"}}, app, dashboard, ""},
	} {
		header := http.Header{}
		for key, values := range c.headers {
			for _, value := range values {
				header.Add(key, value)
			}
		}
		reason, blocked := frameBlocked(header, mustURL(t, c.page), mustURL(t, c.origin))
		assert.Equal(t, reason, c.reason, c.name)
		assert.Equal(t, blocked, c.reason != "", c.name)
	}
}

func TestCheckWebFrame(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	dashboard := mustURL(t, "http://192.168.10.3")

	t.Run("an app that allows it", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		defer server.Close()

		check := checkWebFrame(request, mustURL(t, server.URL), dashboard)
		assert.DeepEqual(t, check, codegen.WebFrameCheck{Embeddable: ptr(true)})
	})

	t.Run("the page a redirect lands on rules", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/" {
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}
			w.Header().Set("X-Frame-Options", "DENY")
		}))
		defer server.Close()

		check := checkWebFrame(request, mustURL(t, server.URL), dashboard)
		assert.DeepEqual(t, check, notEmbeddable(codegen.XFrameOptions))
	})

	t.Run("a certificate nobody vouches for", func(t *testing.T) {
		// httptest's certificate is signed by a CA no system trusts
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		defer server.Close()

		check := checkWebFrame(request, mustURL(t, server.URL), dashboard)
		assert.DeepEqual(t, check, notEmbeddable(codegen.UntrustedCertificate))
	})

	t.Run("plain http under an https dashboard", func(t *testing.T) {
		check := checkWebFrame(request, mustURL(t, "http://192.168.10.3:8080"), mustURL(t, "https://192.168.10.3"))
		assert.DeepEqual(t, check, notEmbeddable(codegen.MixedContent))
	})

	t.Run("an app that does not answer is no answer", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		target := mustURL(t, server.URL)
		server.Close()

		check := checkWebFrame(request, target, dashboard)
		assert.DeepEqual(t, check, codegen.WebFrameCheck{})
	})
}

func TestParseWebURL(t *testing.T) {
	for _, raw := range []string{"", "ftp://box", "file:///etc/passwd", "http://", "192.168.10.3:8443", "javascript:alert(1)"} {
		_, err := parseWebURL(raw)
		assert.Assert(t, err != nil, raw)
	}
	_, err := parseWebURL("https://192.168.10.3:8443/nifi/")
	assert.NilError(t, err)
}

func ptr[T any](v T) *T { return &v }
