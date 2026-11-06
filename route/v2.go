package route

import (
	"crypto/ecdsa"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/ReCasaOS/CasaOS-AppManagement/codegen"
	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/config"

	v2Route "github.com/ReCasaOS/CasaOS-AppManagement/route/v2"
	"github.com/ReCasaOS/CasaOS-Common/external"
	"github.com/ReCasaOS/CasaOS-Common/utils/jwt"
	"github.com/deepmap/oapi-codegen/pkg/middleware"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	echojwt "github.com/labstack/echo-jwt/v4"
	"github.com/labstack/echo/v4"
	echo_middleware "github.com/labstack/echo/v4/middleware"
)

var (
	_swagger *openapi3.T

	V2APIPath string
	V2DocPath string
)

func init() {
	swagger, err := codegen.GetSwagger()
	if err != nil {
		panic(err)
	}

	_swagger = swagger

	u, err := url.Parse(_swagger.Servers[0].URL)
	if err != nil {
		panic(err)
	}

	V2APIPath = strings.TrimRight(u.Path, "/")
	V2DocPath = "/doc" + V2APIPath
}

// isGitWebhook is a forge's delivery, the one route a request reaches without a token: a
// forge proves itself with the app's webhook secret instead, and sends bodies this spec does
// not describe. It is the route echo matched, not the path: echo routes on the raw path, so
// `/git/x%2Fwebhook` decodes to a webhook's path while it is routed to `/git/:app`.
func isGitWebhook(c echo.Context) bool {
	return c.Request().Method == http.MethodPost && c.Path() == V2APIPath+"/git/:app/webhook"
}

// isStatsRulesPut: the handler bounds and checks this body itself, so that any
// fault is the contract's bad_rules and an oversized body is never read whole.
func isStatsRulesPut(c echo.Context) bool {
	return c.Request().Method == http.MethodPut && c.Path() == V2APIPath+"/stats/:id/alerts"
}

// isLogsStream is the one route that takes its token from the query: a browser's
// WebSocket cannot send a header.
func isLogsStream(c echo.Context) bool {
	return c.Request().Method == http.MethodGet && c.Path() == V2APIPath+"/logs/stream"
}

func InitV2Router() http.Handler {
	appManagement := v2Route.NewAppManagement()

	e := echo.New()

	e.Use((echo_middleware.CORSWithConfig(echo_middleware.CORSConfig{
		AllowOrigins:     []string{"*"},
		AllowMethods:     []string{echo.POST, echo.GET, echo.OPTIONS, echo.PUT, echo.DELETE},
		AllowHeaders:     []string{echo.HeaderAuthorization, echo.HeaderContentLength, echo.HeaderXCSRFToken, echo.HeaderContentType, echo.HeaderAccessControlAllowOrigin, echo.HeaderAccessControlAllowHeaders, echo.HeaderAccessControlAllowMethods, echo.HeaderConnection, echo.HeaderOrigin, echo.HeaderXRequestedWith},
		ExposeHeaders:    []string{echo.HeaderContentLength, echo.HeaderAccessControlAllowOrigin, echo.HeaderAccessControlAllowHeaders},
		MaxAge:           172800,
		AllowCredentials: true,
	})))

	e.Use(echo_middleware.Gzip())

	e.Use(echo_middleware.LoggerWithConfig(echo_middleware.LoggerConfig{Format: accessLogFormat}))

	e.Use(echojwt.WithConfig(echojwt.Config{
		Skipper: func(c echo.Context) bool {
			return isGitWebhook(c) || external.IsInternalRequest(c.RealIP(), c.Request().Header.Get(echo.HeaderAuthorization), config.CommonInfo.RuntimePath)
		},
		ParseTokenFunc: func(c echo.Context, token string) (interface{}, error) {
			valid, claims, err := jwt.Validate(token, func() (*ecdsa.PublicKey, error) { return external.GetPublicKey(config.CommonInfo.RuntimePath) })
			if err != nil || !valid {
				return nil, echo.ErrUnauthorized
			}

			c.Request().Header.Set("user_id", strconv.Itoa(claims.ID))

			return claims, nil
		},
		TokenLookupFuncs: []echo_middleware.ValuesExtractor{
			func(c echo.Context) ([]string, error) {
				authorization := c.Request().Header.Get(echo.HeaderAuthorization)
				if authorization == "" && isLogsStream(c) {
					return []string{c.QueryParam("token")}, nil
				}
				return []string{jwt.TokenFromAuthorization(authorization)}, nil
			},
		},
	}))

	// e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
	// 	return func(c echo.Context) error {
	// 		switch c.Request().Header.Get(echo.HeaderContentType) {
	// 		case common.MIMEApplicationYAML: // in case request contains a compose content in YAML
	// 			return middleware.OapiRequestValidatorWithOptions(_swagger, &middleware.Options{
	// 				Options: openapi3filter.Options{
	// 					AuthenticationFunc: openapi3filter.NoopAuthenticationFunc,
	// 					// ExcludeRequestBody:  true,
	// 					// ExcludeResponseBody: true,
	// 				},
	// 			})(next)(c)

	// 		default:
	// 			return middleware.OapiRequestValidatorWithOptions(_swagger, &middleware.Options{
	// 				Options: openapi3filter.Options{
	// 					AuthenticationFunc: openapi3filter.NoopAuthenticationFunc,
	// 				},
	// 			})(next)(c)
	// 		}
	// 	}
	// })

	e.Use(middleware.OapiRequestValidatorWithOptions(_swagger, &middleware.Options{
		Options: openapi3filter.Options{
			AuthenticationFunc: openapi3filter.NoopAuthenticationFunc,
			// The dashboard edits what it read and sends it back whole, readOnly
			// fields included (a schedule's last_run); the handlers ignore those and
			// keep their own. kin-openapi refuses them by default since the version
			// this project moved to, which made every schedule unsavable
			// (ReCasaOS/CasaOS#7).
			ExcludeReadOnlyValidations: true,
		},
		Skipper: func(c echo.Context) bool { return isGitWebhook(c) || isStatsRulesPut(c) },
	}))

	codegen.RegisterHandlersWithBaseURL(e, appManagement, V2APIPath)

	return e
}

func InitV2DocRouter(docHTML string, docYAML string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == V2DocPath {
			if _, err := w.Write([]byte(docHTML)); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
			}
			return
		}

		if r.URL.Path == V2DocPath+"/openapi.yaml" {
			if _, err := w.Write([]byte(docYAML)); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
			}
		}
	})
}
