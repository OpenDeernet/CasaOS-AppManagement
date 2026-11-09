package service

import (
	"fmt"

	"github.com/ReCasaOS/CasaOS-AppManagement/model"
	v1 "github.com/ReCasaOS/CasaOS-AppManagement/service/v1"
)

// ValidateLabels checks the labels the user put on the services (see model/labels.go for the
// rules). known is the app as it already is, the installed one when settings are saved, the
// store's version when it is installed: what it carries is not checked again. The icon label
// this box writes from x-casaos is not the user's either.
func (a *ComposeApp) ValidateLabels(known *ComposeApp) error {
	icon := ""
	if info, err := a.StoreInfo(false); err == nil && info != nil {
		icon = info.Icon
	}

	for _, name := range sortedServiceNames(a.Services) {
		labels := a.Services[name].Labels
		if icon != "" && labels[v1.V1LabelIcon] == icon {
			withoutIcon := make(map[string]string, len(labels))
			for key, value := range labels {
				if key != v1.V1LabelIcon {
					withoutIcon[key] = value
				}
			}
			labels = withoutIcon
		}

		var before map[string]string
		if known != nil {
			before = known.Services[name].Labels
		}

		if err := model.ValidateLabels(labels, before); err != nil {
			return fmt.Errorf("service %s: %w", name, err)
		}
	}

	return nil
}

// StoreVersion is the store's copy of the app, nil when the store has none.
func (a *ComposeApp) StoreVersion() *ComposeApp {
	id := a.Name
	if info, err := a.StoreInfo(false); err == nil && info != nil && info.StoreAppID != nil && *info.StoreAppID != "" {
		id = *info.StoreAppID
	}

	storeApp, err := MyService.AppStoreManagement().ComposeApp(id)
	if err != nil {
		return nil
	}
	return storeApp
}
