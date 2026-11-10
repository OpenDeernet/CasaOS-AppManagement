package v2

import (
	"encoding/json"
	"testing"

	"github.com/ReCasaOS/CasaOS-AppManagement/codegen"
	"github.com/ReCasaOS/CasaOS-AppManagement/common"
	"gotest.tools/v3/assert"
)

func TestUpgradableAppTitleUsesDefaultLanguageNotJSON(t *testing.T) {
	storeInfo := &codegen.ComposeAppStoreInfo{
		Title: map[string]string{
			common.DefaultLanguage: "Nextcloud",
			"zh_cn":                "Nextcloud",
		},
	}

	// The old path put the marshalled map into the string field.
	marshalled, err := json.Marshal(storeInfo.Title)
	assert.NilError(t, err)
	assert.Assert(t, string(marshalled) != "Nextcloud")

	got := upgradableAppTitle(storeInfo, "nextcloud-app")
	assert.Equal(t, got, "Nextcloud")
	assert.Assert(t, got != string(marshalled))
}

func TestUpgradableAppTitleFallsBackToComposeName(t *testing.T) {
	assert.Equal(t, upgradableAppTitle(&codegen.ComposeAppStoreInfo{}, "my-app"), "my-app")
	assert.Equal(t, upgradableAppTitle(nil, "my-app"), "my-app")
}
