package v1_test

import (
	"testing"

	"github.com/ReCasaOS/CasaOS-AppManagement/model"
	v1 "github.com/ReCasaOS/CasaOS-AppManagement/service/v1"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/stretchr/testify/assert"
)

func containerWith(labels map[string]string) types.ContainerJSON {
	return types.ContainerJSON{
		ContainerJSONBase: &types.ContainerJSONBase{Name: "/demo", HostConfig: &container.HostConfig{}},
		Config:            &container.Config{Labels: labels},
	}
}

// what the wizard shows of a container are the user's labels: not what this box set, not what
// would be refused when they come back
func TestAContainersCustomLabelsLeaveOutTheSystemsAndTheRefused(t *testing.T) {
	m := v1.GetCustomizationPostData(containerWith(map[string]string{
		"casaos":                    "casaos",
		"origin":                    "custom",
		"icon":                      "http://x/i.png",
		"io.casaos.v1.app.store.id": "3",
		"traefik.enable":            "true",
		"homepage.group":            "Media",
		"not valid":                 "x",
	}))

	assert.Equal(t, model.EnvArray{
		{Name: "homepage.group", Value: "Media"},
		{Name: "traefik.enable", Value: "true"},
	}, m.CustomLabels)

	// the way back: what was shown is accepted as it is, and ends up as labels of the service
	assert.NoError(t, m.ValidateCustomLabels())
	assert.Equal(t, map[string]string{"homepage.group": "Media", "traefik.enable": "true"},
		map[string]string(m.Services()["demo"].Labels))
}

func TestAContainerWithoutCustomLabelsHasNone(t *testing.T) {
	m := v1.GetCustomizationPostData(containerWith(map[string]string{"casaos": "casaos"}))
	assert.Empty(t, m.CustomLabels)
	assert.NoError(t, m.ValidateCustomLabels())
}
