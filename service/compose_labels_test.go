package service_test

import (
	"testing"

	"github.com/ReCasaOS/CasaOS-AppManagement/service"
	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"gotest.tools/v3/assert"
)

func composeWithLabels(t *testing.T, labels string) *service.ComposeApp {
	t.Helper()
	logger.LogInitConsoleOnly()

	text := `name: demo
services:
  web:
    image: nginx
    labels:
` + labels + `x-casaos:
  main: web
  icon: https://example.com/demo.png
  title:
    en_us: Demo
`
	composeApp, err := service.NewComposeAppFromYAML([]byte(text), true, true)
	assert.NilError(t, err)
	return composeApp
}

func TestLabelsOfTheWizardAreAccepted(t *testing.T) {
	// as a list, the way ComposeConfig.vue writes them, and as a map
	for _, labels := range []string{
		"      - traefik.enable=true\n      - traefik.http.routers.demo.rule=Host(`demo.lan`)\n",
		"      traefik.enable: \"true\"\n      traefik.http.routers.demo.rule: Host(`demo.lan`)\n",
	} {
		assert.NilError(t, composeWithLabels(t, labels).ValidateLabels(nil))
	}
}

// the icon label is written by this box from x-casaos on every service: not the user's
func TestTheIconLabelOfTheSystemIsNotRefused(t *testing.T) {
	composeApp := composeWithLabels(t, "      traefik.enable: \"true\"\n")
	assert.Equal(t, composeApp.Services["web"].Labels["icon"], "https://example.com/demo.png")
	assert.NilError(t, composeApp.ValidateLabels(nil))
}

func TestALabelOfTheSystemsIsRefused(t *testing.T) {
	for _, label := range []string{"casaos: casaos", "origin: system", "io.casaos.v1.app.store.id: \"1\"", "com.docker.compose.project: other"} {
		err := composeWithLabels(t, "      "+label+"\n").ValidateLabels(nil)
		assert.ErrorContains(t, err, "service web:")
		assert.ErrorContains(t, err, "cannot be used")
	}
}

func TestAnInvalidLabelIsRefused(t *testing.T) {
	assert.ErrorContains(t, composeWithLabels(t, "      \"has space\": x\n").ValidateLabels(nil), "may only have")
	assert.ErrorContains(t, composeWithLabels(t, "      a: \"line\\nbreak\"\n").ValidateLabels(nil), "control character")
}

// saved as it is, an app keeps what it came with, whatever that is; what is new or changed is checked
func TestLabelsTheAppAlreadyHasAreNotRefused(t *testing.T) {
	installed := composeWithLabels(t, "      \"legacy key\": x\n      origin: custom\n")
	same := composeWithLabels(t, "      \"legacy key\": x\n      origin: custom\n")
	assert.NilError(t, same.ValidateLabels(installed))

	changed := composeWithLabels(t, "      \"legacy key\": x\n      origin: system\n")
	assert.ErrorContains(t, changed.ValidateLabels(installed), `"origin"`)

	added := composeWithLabels(t, "      \"legacy key\": x\n      origin: custom\n      casaos: casaos\n")
	assert.ErrorContains(t, added.ValidateLabels(installed), `"casaos"`)
}
