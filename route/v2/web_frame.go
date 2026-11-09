package v2

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ReCasaOS/CasaOS-AppManagement/codegen"
	"github.com/labstack/echo/v4"
	"github.com/samber/lo"
)

// How long an app gets to answer. The dashboard waits on this before it opens
// anything, so it is short: an app that has not answered by then is opened the
// way the setting says, which is what happened before anything was asked.
const webFrameCheckTimeout = 3 * time.Second

// CheckWebFrame answers whether the browser will render an app's web interface
// in a frame on the dashboard.
//
// A browser that refuses a frame says so to nobody: the page holding it gets no
// error and no event, only an empty frame. NiFi was the case that showed it:
// `X-Frame-Options: SAMEORIGIN` on https://<box>:8443 under a dashboard on
// http://<box>, and a self-signed certificate that a frame offers no way to
// accept. The dashboard cannot read another origin's headers, so this does.
func (a *AppManagement) CheckWebFrame(ctx echo.Context, params codegen.CheckWebFrameParams) error {
	target, err := parseWebURL(params.Url)
	if err != nil {
		message := "url: " + err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}
	origin, err := parseWebURL(params.Origin)
	if err != nil {
		message := "origin: " + err.Error()
		return ctx.JSON(http.StatusBadRequest, codegen.ResponseBadRequest{Message: &message})
	}

	return ctx.JSON(http.StatusOK, codegen.WebFrameCheckOK{
		Data: lo.ToPtr(checkWebFrame(ctx.Request(), target, origin)),
	})
}

func parseWebURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errNotWebURL
	}
	if parsed.Hostname() == "" {
		return nil, errNotWebURL
	}
	return parsed, nil
}

type webURLError string

func (e webURLError) Error() string { return string(e) }

const errNotWebURL = webURLError("not an http or https URL with a host")

func checkWebFrame(request *http.Request, target, origin *url.URL) codegen.WebFrameCheck {
	// The browser refuses this before it asks the app anything.
	if origin.Scheme == "https" && target.Scheme == "http" {
		return notEmbeddable(codegen.MixedContent)
	}

	// Every hop is verified, the one a redirect lands on as much as the first: the
	// browser fails the frame on whichever of them it cannot trust. The handshake
	// itself never fails, so the headers can still be read.
	var untrusted atomic.Bool
	client := &http.Client{
		Timeout: webFrameCheckTimeout,
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: (&net.Dialer{
				Timeout: webFrameCheckTimeout,
			}).DialContext,
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, //nolint:gosec // verified by hand below, only to report it
				VerifyConnection: func(state tls.ConnectionState) error {
					if !certificateTrusted(state) {
						untrusted.Store(true)
					}
					return nil
				},
			},
		},
	}
	defer client.CloseIdleConnections()

	probe, err := http.NewRequestWithContext(request.Context(), http.MethodGet, target.String(), nil)
	if err != nil {
		return codegen.WebFrameCheck{}
	}
	// some apps answer a client that does not look like a browser with something else
	probe.Header.Set("User-Agent", "Mozilla/5.0 (compatible; CasaOS frame check)")
	probe.Header.Set("Accept", "text/html")

	response, err := client.Do(probe)
	if err != nil {
		// not an answer either way; the dashboard opens the app the way it always did
		return codegen.WebFrameCheck{}
	}
	response.Body.Close()

	if untrusted.Load() {
		return notEmbeddable(codegen.UntrustedCertificate)
	}

	// the frame shows what the last redirect landed on, and that page's headers rule
	if reason, blocked := frameBlocked(response.Header, response.Request.URL, origin); blocked {
		return notEmbeddable(reason)
	}

	return codegen.WebFrameCheck{Embeddable: lo.ToPtr(true)}
}

func notEmbeddable(reason codegen.WebFrameCheckReason) codegen.WebFrameCheck {
	return codegen.WebFrameCheck{Embeddable: lo.ToPtr(false), Reason: &reason}
}

// certificateTrusted is what a browser without a private CA would decide. One the
// owner has added to their browser but not to this machine reads as untrusted,
// and the app opens in a tab: one click more, never an empty frame.
func certificateTrusted(state tls.ConnectionState) bool {
	if len(state.PeerCertificates) == 0 {
		return false
	}
	intermediates := x509.NewCertPool()
	for _, certificate := range state.PeerCertificates[1:] {
		intermediates.AddCert(certificate)
	}
	_, err := state.PeerCertificates[0].Verify(x509.VerifyOptions{
		DNSName:       state.ServerName,
		Intermediates: intermediates,
	})
	return err == nil
}

// frameBlocked applies the browser's rules to one response's headers: the CSP's
// `frame-ancestors` where any policy has one, `X-Frame-Options` only where none
// does (a browser ignores the older header once the newer one speaks).
func frameBlocked(header http.Header, page, origin *url.URL) (codegen.WebFrameCheckReason, bool) {
	ancestorsSeen := false
	for _, policy := range header.Values("Content-Security-Policy") {
		// one header can carry several policies, comma-separated; each must allow it
		for _, single := range strings.Split(policy, ",") {
			sources, ok := frameAncestors(single)
			if !ok {
				continue
			}
			ancestorsSeen = true
			if !ancestorsAllow(sources, page, origin) {
				return codegen.FrameAncestors, true
			}
		}
	}
	if ancestorsSeen {
		return "", false
	}

	for _, value := range header.Values("X-Frame-Options") {
		for _, option := range strings.Split(value, ",") {
			switch strings.ToLower(strings.TrimSpace(option)) {
			case "deny":
				return codegen.XFrameOptions, true
			case "sameorigin":
				if !sameOrigin(page, origin) {
					return codegen.XFrameOptions, true
				}
			}
			// ALLOW-FROM and anything else: today's browsers ignore it
		}
	}

	return "", false
}

// frameAncestors returns the sources of a policy's `frame-ancestors`, and whether
// it has one at all.
func frameAncestors(policy string) ([]string, bool) {
	for _, directive := range strings.Split(policy, ";") {
		fields := strings.Fields(directive)
		if len(fields) > 0 && strings.EqualFold(fields[0], "frame-ancestors") {
			return fields[1:], true
		}
	}
	return nil, false
}

func ancestorsAllow(sources []string, page, origin *url.URL) bool {
	for _, source := range sources {
		lower := strings.ToLower(source)
		switch {
		case lower == "'none'":
			// matches nothing; next to other sources it is ignored
		case lower == "'self'":
			if sameOrigin(page, origin) {
				return true
			}
		case lower == "*":
			return true
		case strings.HasSuffix(lower, ":") && !strings.Contains(lower, "/"):
			// a scheme source; http: also lets an https ancestor through
			if schemeMatches(strings.TrimSuffix(lower, ":"), origin.Scheme) {
				return true
			}
		default:
			if hostSourceMatches(lower, page, origin) {
				return true
			}
		}
	}
	return false
}

// hostSourceMatches handles `[scheme://]host[:port][/path]`, host maybe `*.`-prefixed,
// port maybe `*`. A path cannot narrow an ancestor, which is an origin, so it is dropped.
func hostSourceMatches(source string, page, origin *url.URL) bool {
	scheme := ""
	if before, after, found := strings.Cut(source, "://"); found {
		scheme, source = before, after
	}
	if slash := strings.Index(source, "/"); slash >= 0 {
		source = source[:slash]
	}
	host, port := source, ""
	if colon := strings.LastIndex(source, ":"); colon >= 0 && !strings.HasSuffix(source, "]") {
		host, port = source[:colon], source[colon+1:]
	}

	if scheme == "" {
		// a source without a scheme takes the page's
		scheme = page.Scheme
	}
	if !schemeMatches(scheme, origin.Scheme) {
		return false
	}

	originHost := strings.ToLower(origin.Hostname())
	switch {
	case strings.HasPrefix(host, "*."):
		if !strings.HasSuffix(originHost, host[1:]) {
			return false
		}
	case host != originHost:
		return false
	}

	switch port {
	case "*":
		return true
	case "":
		// the default port of the source's scheme, or 443 for http upgraded
		return effectivePort(origin) == defaultPort(scheme) || effectivePort(origin) == defaultPort(origin.Scheme)
	default:
		return port == effectivePort(origin)
	}
}

func schemeMatches(source, ancestor string) bool {
	return source == ancestor || (source == "http" && ancestor == "https")
}

func sameOrigin(a, b *url.URL) bool {
	return a.Scheme == b.Scheme &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectivePort(a) == effectivePort(b)
}

func effectivePort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	return defaultPort(u.Scheme)
}

func defaultPort(scheme string) string {
	switch scheme {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}
