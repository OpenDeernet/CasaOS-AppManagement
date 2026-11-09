package model_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ReCasaOS/CasaOS-AppManagement/common"
	"github.com/ReCasaOS/CasaOS-AppManagement/model"
	"github.com/ReCasaOS/CasaOS-AppManagement/service"
	"github.com/ReCasaOS/CasaOS-Common/utils/logger"
	"github.com/stretchr/testify/assert"
	"gopkg.in/yaml.v3"
)

func TestLabelsOfAnAppFileReachTheComposeApp(t *testing.T) {
	logger.LogInitConsoleOnly()
	var appFile model.CustomizationPostData
	assert.NoError(t, json.Unmarshal([]byte(common.SampleLegacyAppfileExportJSON), &appFile))
	assert.NoError(t, json.Unmarshal([]byte(`{"custom_labels":[
		{"container":"traefik.enable","host":"true"},
		{"container":"homepage.group","host":"Media"}]}`), &appFile))
	assert.NoError(t, appFile.ValidateCustomLabels())

	buf, err := yaml.Marshal(appFile.Compose())
	assert.NoError(t, err)

	composeApp, err := service.NewComposeAppFromYAML(buf, true, true)
	assert.NoError(t, err)
	labels := map[string]string(composeApp.Services[strings.ToLower(appFile.ContainerName)].Labels)
	assert.NotEmpty(t, labels["icon"], "the icon label is the system's, written from x-casaos")
	delete(labels, "icon")
	assert.Equal(t, map[string]string{"traefik.enable": "true", "homepage.group": "Media"}, labels)
}

func TestAnAppFileWithoutLabelsGetsNone(t *testing.T) {
	var appFile model.CustomizationPostData
	assert.NoError(t, json.Unmarshal([]byte(common.SampleLegacyAppfileExportJSON), &appFile))
	assert.NoError(t, appFile.ValidateCustomLabels())
	for _, svc := range appFile.Services() {
		assert.Empty(t, svc.Labels)
	}
}

// compose takes labels as a map or as a list; both come out the same
func TestComposeLabelsAsMapAndAsList(t *testing.T) {
	logger.LogInitConsoleOnly()
	for name, labels := range map[string]string{
		"map":  "    labels:\n      a.b: \"1\"\n      c: two\n",
		"list": "    labels:\n      - a.b=1\n      - c=two\n",
	} {
		composeApp, err := service.NewComposeAppFromYAML([]byte("name: demo\nservices:\n  web:\n    image: nginx\n"+labels), true, true)
		assert.NoError(t, err, name)
		assert.Equal(t, map[string]string{"a.b": "1", "c": "two"}, map[string]string(composeApp.Services["web"].Labels), name)
	}
}

func TestLabelsRefused(t *testing.T) {
	refused := map[string]struct{ key, value string }{
		"no name":              {"", "x"},
		"space in name":        {"my label", "x"},
		"equals in name":       {"a=b", "x"},
		"colon in name":        {"a:b", "x"},
		"dollar in name":       {"a$b", "x"},
		"starts with a dot":    {".a", "x"},
		"name too long":        {strings.Repeat("a", model.MaxLabelKeyLength+1), "x"},
		"value too long":       {"a", strings.Repeat("v", model.MaxLabelValueLength+1)},
		"newline in value":     {"a", "x\ny: z"},
		"nul in value":         {"a", "x\x00"},
		"not UTF-8":            {"a", "\xff\xfe"},
		"casaos":               {"casaos", "casaos"},
		"origin":               {"origin", "system"},
		"icon":                 {"icon", "http://elsewhere/x.png"},
		"reserved in any case": {"CasaOS", "casaos"},
		"io.casaos namespace":  {"io.casaos.v1.app.store.id", "1"},
		"compose project":      {"com.docker.compose.project", "other"},
		"compose namespace":    {"com.docker.compose.service", "other"},
	}
	for name, l := range refused {
		assert.Error(t, model.ValidateLabel(l.key, l.value), name)
		assert.Error(t, model.ValidateLabels(map[string]string{l.key: l.value}, nil), name)
	}
}

func TestLabelsAccepted(t *testing.T) {
	for key, value := range map[string]string{
		"traefik.http.routers.my-app.rule":                      "Host(`app.example.com`)",
		"traefik.http.services.my_app.loadbalancer.server.port": "8080",
		"org.opencontainers.image.source":                       "https://example.com/x",
		"com.centurylinklabs.watchtower.enable":                 "false",
		"app.kubernetes.io/name":                                "démo",
		"empty":                                                 "",
		"com.docker.desktop":                                    "x",
	} {
		assert.NoError(t, model.ValidateLabel(key, value), key)
	}
}

func TestAnAppFileNamingALabelTwiceOrAReservedOneIsRefused(t *testing.T) {
	for name, body := range map[string]string{
		"twice":    `{"custom_labels":[{"container":"a","host":"1"},{"container":"a","host":"2"}]}`,
		"reserved": `{"custom_labels":[{"container":"casaos","host":"x"}]}`,
		"bad key":  `{"custom_labels":[{"container":"a b","host":"x"}]}`,
	} {
		var appFile model.CustomizationPostData
		assert.NoError(t, json.Unmarshal([]byte(body), &appFile), name)
		assert.Error(t, appFile.ValidateCustomLabels(), name)
	}
}

func TestTooManyLabels(t *testing.T) {
	labels := map[string]string{}
	for i := 0; i < model.MaxLabelsPerService; i++ {
		labels["k"+strings.Repeat("a", i)] = "v"
	}
	assert.NoError(t, model.ValidateLabels(labels, nil))

	labels["one-more"] = "v"
	assert.Error(t, model.ValidateLabels(labels, nil))

	// the ones the service already carries do not count
	known := map[string]string{}
	for k, v := range labels {
		known[k] = v
	}
	assert.NoError(t, model.ValidateLabels(labels, known))
}

// an app that came with a label of its own, or one this box added, is saved as it is; a
// reserved name is refused as soon as it is new or its value is changed
func TestLabelsTheServiceAlreadyHasAreNotCheckedAgain(t *testing.T) {
	known := map[string]string{"icon": "http://store/icon.png", "bad key": "x", "traefik.enable": "true"}

	assert.NoError(t, model.ValidateLabels(map[string]string{"icon": "http://store/icon.png", "bad key": "x", "traefik.enable": "true"}, known))
	assert.NoError(t, model.ValidateLabels(map[string]string{"traefik.enable": "false"}, known))
	assert.Error(t, model.ValidateLabels(map[string]string{"icon": "http://elsewhere/x.png"}, known))
	assert.Error(t, model.ValidateLabels(map[string]string{"bad key": "y"}, known))
	assert.Error(t, model.ValidateLabels(map[string]string{"casaos": "casaos"}, known))
}
