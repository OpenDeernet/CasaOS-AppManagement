package route

import (
	"crypto/ecdsa"
	"net/http"
	"strconv"

	"github.com/ReCasaOS/CasaOS-AppManagement/pkg/config"
	v1 "github.com/ReCasaOS/CasaOS-AppManagement/route/v1"
	"github.com/ReCasaOS/CasaOS-Common/external"
	"github.com/ReCasaOS/CasaOS-Common/utils/jwt"
	echojwt "github.com/labstack/echo-jwt/v4"
	"github.com/labstack/echo/v4"
	echo_middleware "github.com/labstack/echo/v4/middleware"
)

const (
	V1APIPath = "/v1/app_management"
	V1DocPath = "/v1doc" + V1APIPath
)

func InitV1Router() http.Handler {
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
	e.Use(echo_middleware.Recover())
	e.Use(echo_middleware.LoggerWithConfig(echo_middleware.LoggerConfig{Format: accessLogFormat}))

	v1Group := e.Group("/v1")

	v1Group.Use(echojwt.WithConfig(echojwt.Config{
		Skipper: func(c echo.Context) bool {
			return external.IsInternalRequest(c.RealIP(), c.Request().Header.Get(echo.HeaderAuthorization), config.CommonInfo.RuntimePath)
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
				if len(c.Request().Header.Get(echo.HeaderAuthorization)) > 0 {
					return []string{jwt.TokenFromAuthorization(c.Request().Header.Get(echo.HeaderAuthorization))}, nil
				}
				return []string{c.QueryParam("token")}, nil
			},
		},
	}))
	{
		v1ContainerGroup := v1Group.Group("/container")
		v1ContainerGroup.Use()
		{

			// v1ContainerGroup.GET("", v1.MyAppList) ///my/list
			v1ContainerGroup.GET("/usage", v1.AppUsageList)
			v1ContainerGroup.GET("/:id", v1.ContainerUpdateInfo)   ///update/:id/info
			v1ContainerGroup.GET("/:id/compose", v1.ToComposeYAML) // /app/setting/:id
			// v1ContainerGroup.GET("/:id/logs", v1.ContainerLog)        // /app/logs/:id
			v1ContainerGroup.GET("/networks", v1.GetDockerNetworks)   // /app/install/config
			v1ContainerGroup.PUT("/archive/:id", v1.ArchiveContainer) // /container/archive/:id

			// v1ContainerGroup.GET("/:id/state", v1.GetContainerState) // app/state/:id ?state=install_progress
			// there are problems, temporarily do not deal with
			v1ContainerGroup.GET("/:id/terminal", v1.DockerTerminal) // app/terminal/:id
			// v1ContainerGroup.POST("", v1.InstallApp)                 // app/install

			v1ContainerGroup.PUT("/:id", v1.UpdateSetting) ///update/:id/setting

			// v1ContainerGroup.PUT("/:id/state", v1.ChangAppState) // /app/state/:id
			v1ContainerGroup.DELETE("/:id", v1.UninstallApp) // app/uninstall/:id

			// where Docker's data is, and moving it: before /:id, which would take them
			v1ContainerGroup.GET("/docker-location", v1.GetDockerLocation)
			v1ContainerGroup.GET("/docker-location/check", v1.CheckDockerLocation)
			v1ContainerGroup.POST("/docker-location", v1.MoveDockerLocation)
			v1ContainerGroup.DELETE("/docker-location/old", v1.DeleteDockerLocationOldCopy)

			// v1ContainerGroup.GET("/info", v1.GetDockerDaemonConfiguration)
			// v1ContainerGroup.PUT("/info", v1.PutDockerDaemonConfiguration)
		}
	}

	return e
}

func InitV1DocRouter(docHTML string, docYAML string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == V1DocPath {
			if _, err := w.Write([]byte(docHTML)); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
			}
			return
		}

		if r.URL.Path == V1DocPath+"/openapi_v1.yaml" {
			if _, err := w.Write([]byte(docYAML)); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
			}
		}
	})
}
